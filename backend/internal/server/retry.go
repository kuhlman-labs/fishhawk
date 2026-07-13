package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryStageRetried is the audit-log category for the chained
// entry the retry handler writes when a stage re-opens. The
// payload carries the prior failure category and reason so the
// audit trail records what was retried (without forcing a reader
// to walk back to the prior `stage_failed`-shaped entries).
const CategoryStageRetried = "stage_retried"

// CategoryStageOverrideRetried is the audit-log category for the
// chained entry the retry handler writes when an operator re-opens a
// genuine category-B stage via the {override:true} escape hatch
// (#698). It is kept DISTINCT from stage_retried so the explicit
// override (who/why) stays separable in audit analysis from both an
// ordinary retry and #692's automatic empty-diff → C reclassification.
// The override re-opens the B stage to pending: the stage re-runs and
// the policy gate re-evaluates the new diff — it does not accept the
// B-violating diff or bypass the gate.
const CategoryStageOverrideRetried = "stage_override_retried"

// retryRequest is the optional JSON body of POST
// /v0/stages/{stage_id}/retry. An empty body retries with default
// per-category semantics. {override:true} admits a category-B
// failure onto the A/C re-open path and REQUIRES a non-empty reason.
type retryRequest struct {
	Override bool   `json:"override"`
	Reason   string `json:"reason"`
	// Delegated opts the retry into the ADR-040 delegated-action path
	// (#1026): checkDelegation re-evaluates the operator_agent may_retry
	// condition (infra_flake) server-side at action time — 403
	// delegation_not_configured / delegation_condition_unmet on refusal,
	// `delegated: "<rule>"` on the retry audit payload when met. Absent
	// → behavior byte-identical to today.
	Delegated bool `json:"delegated"`
}

// handleRetryStage implements POST /v0/stages/{stage_id}/retry.
//
// Per-category retry semantics (E8.3 #146 + E8.6 #173):
//
//	A (agent failure)            → 200; failed → pending →
//	                               (orchestrator) → dispatched.
//	                               workflow_dispatch fires for the
//	                               same workflow_id + workflow_sha.
//	B (constraint/policy)        → 422; the workflow or spec
//	                               needs to change first.
//	C (infrastructure)           → 200; same flow as A — fresh
//	                               runner instance with a fresh
//	                               signing key.
//	D, sla_timeout sub-reason    → 200; failed → awaiting_approval,
//	                               failure metadata cleared,
//	                               updated_at trigger restarts the
//	                               SLA clock for the next ticker
//	                               pass.
//	D, gate-rejected sub-reason  → 422; the approver said no, a
//	                               fresh run is the right next
//	                               step.
//
// The high-level decision tree lives in run.RetryStage; this
// handler is the HTTP shim around it. For A/C the handler also
// invokes the orchestrator after the state transition (and after
// the audit write) to fire the actual workflow_dispatch.
// Orchestrator failures are logged but don't fail the request:
// the audit row is in place, the stage is in pending, an operator
// can re-fire Advance manually if needed.
func (s *Server) handleRetryStage(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:retries") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:retries",
			map[string]any{"required_scope": "write:stages or write:retries"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "retry_unconfigured",
			"retry endpoint requires run + audit repositories", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	// Optional request body. Absent body → default per-category retry.
	// {override:true} admits a category-B failure onto the re-open path
	// and requires a recorded reason (the audited escape hatch, #698).
	var reqBody retryRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {override, reason}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if reqBody.Override && strings.TrimSpace(reqBody.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required when override is set",
			map[string]any{"field": "reason"})
		return
	}

	// Pre-fetch the stage to get the RunID for subject-binding guard.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	// Acceptance-reopen branch (E31.16 / #1567). A failed acceptance VERDICT
	// leaves the acceptance STAGE `succeeded` (E31.7), so the ordinary
	// failed-stages-only retry path below can never re-run an acceptance stage
	// that settled succeeded but recorded NO verdict (the run-f7a4b71b hole:
	// the agent emitted a non-schema field, the ship failed closed, and the
	// stage settled with no outcome). This operator-gated verb re-opens such a
	// stage to pending via run.ReopenAcceptanceStage — the E31.8 class-2
	// mechanic — WITHOUT widening the failed-stages-only run.RetryStage
	// invariant. Handled entirely here, before the subject-binding guard and
	// retryStageAs, so the verdict-ful acceptance failure keeps routing through
	// the deterministic triage untouched.
	if stage.Type == run.StageTypeAcceptance && stage.State == run.StageStateSucceeded {
		s.retryAcceptanceOutcomeUnknown(w, r, id, stage)
		return
	}

	// Subject-binding guard: MCP tokens may only retry stages within
	// their own run. Subject format is "mcp:run:<uuid>" (set by
	// bearerAuth middleware at middleware.go).
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		// The category-B override is an OPERATOR-only escape hatch (#698):
		// re-opening a genuine policy-gate failure is a human decision, not
		// an agent's. Reject any agent (MCP subject-bound) token outright
		// when override is set — even for the agent's own run — mirroring
		// the /redrive guard, so the stage_override_retried audit's operator
		// attribution holds (writeOverrideRetryAudit's ActorUser is then
		// correct by construction). Normal (non-override) retry stays
		// subject-bound below so agents can still retry their own A/C
		// failures.
		if reqBody.Override {
			s.writeError(w, r, http.StatusForbidden, "agent_token_forbidden",
				"the category-B override is an operator-only action; agent tokens may not invoke it",
				nil)
			return
		}
		runIDStr := strings.TrimPrefix(id.Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != stage.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_retry",
				"mcp token may only retry stages within its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"stage_run_id": stage.RunID.String(),
				})
			return
		}
	}

	// Delegated-action enforcement (ADR-040 / #1026): a delegated:true
	// retry must hold the may_retry condition (infra_flake) against
	// CURRENT run state, re-evaluated server-side before any state
	// change. The condition requires a category-A infra-flake failure,
	// so a delegated category-B override can never pass it.
	var delegatedRule string
	if reqBody.Delegated {
		rule, ok := s.checkDelegation(w, r, stage.RunID, delegation.ActionRetry)
		if !ok {
			return
		}
		delegatedRule = rule
	}

	// Gate-action core (E25.6 / ADR-047): the transition + audit + run
	// un-terminal + orchestrator handoff + drive stamp + status notify is
	// factored into retryStageAs, an identity-parameterised service method
	// the in-process campaign auto-driver also calls. run.RetryStage's
	// sentinel errors are returned verbatim and mapped to HTTP here exactly
	// as before.
	stageOut, err := s.retryStageAs(r.Context(), id, retryActionParams{
		StageID:        stageID,
		Override:       reqBody.Override,
		OverrideReason: reqBody.Reason,
		DelegatedRule:  delegatedRule,
	})
	if err != nil {
		switch {
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		case errors.Is(err, run.ErrRetryNotImplemented):
			// No path returns this as of E8.6, but keep the mapping
			// so callers that switch on it stay sane.
			s.writeError(w, r, http.StatusNotImplemented, "retry_not_implemented",
				err.Error(), nil)
			return
		case errors.Is(err, run.ErrRetryNotApplicable):
			s.writeError(w, r, http.StatusUnprocessableEntity, "retry_not_applicable",
				err.Error(), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"retry failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, toStageResponse(stageOut))
}

// retryAcceptanceOutcomeUnknown handles the acceptance-reopen arm of POST
// /v0/stages/{stage_id}/retry (E31.16 / #1567): a SUCCEEDED acceptance stage
// with NO recorded outcome re-opens to pending for a re-run. It is
// operator-gated (agent tokens refused), guards against reopening a
// verdict-ful stage (that routing belongs to the deterministic triage), and
// fails closed on an audit read error so a re-open never fires on unknown
// evidence state. On success it re-opens via run.ReopenAcceptanceStage, writes
// an acceptance_reopened chained audit entry audit-first, then mirrors
// routeAcceptanceClass2's post-steps (orchestrator Advance, status notify).
func (s *Server) retryAcceptanceOutcomeUnknown(w http.ResponseWriter, r *http.Request, id Identity, stage *run.Stage) {
	ctx := r.Context()

	// (i) Operator-gated: an agent (mcp:run:*-subject) token may not invoke
	// the reopen — mirroring the category-B override guard. Reopening a
	// settled acceptance gate is a human decision.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		s.writeError(w, r, http.StatusForbidden, "agent_token_forbidden",
			"the acceptance-reopen retry is an operator-only action; agent tokens may not invoke it",
			nil)
		return
	}

	// (ii) Head-aware admit (E31.16 #1567 + #1682 Option C). The base rule: a
	// stage with NO acceptance_outcome_recorded verdict may reopen (the #1567
	// outcome-unknown hole); a verdict-ful stage normally belongs to the
	// deterministic triage (422). The #1682 exception threads through here:
	// when a fix-up push landed a NEW head AFTER the verdict was recorded, the
	// verdict is bound to a STALE commit — admit the re-open so acceptance
	// re-validates the final commit, rather than a blanket 422. Staleness is
	// exactly recorded_head != current_head.
	//
	// Fail closed to today's 422 on every uncertainty so a re-open never fires
	// on ambiguous evidence: the recorded verdict has no head_sha (a pre-#1682
	// entry), the current head cannot be resolved, or the two heads are equal.
	// A read error means we cannot prove the stage state, so refuse (500). This
	// arm is genuinely reachable independent of Option A (binding condition 3):
	// it admits a stale-head verdict reached via ANY route, keyed only on the
	// recorded-vs-current head mismatch, not on a pre-reopen timing window.
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, stage.RunID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list acceptance outcome audit entries failed",
			map[string]any{"error": err.Error()})
		return
	}
	recordedHead, hasVerdict := latestAcceptanceVerdictHead(entries, stage.ID)
	if hasVerdict {
		currentHead, currentOK, headErr := s.latestRunHeadSHA(ctx, stage.RunID)
		if headErr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resolve current head for acceptance retry failed",
				map[string]any{"error": headErr.Error()})
			return
		}
		staleHead := recordedHead != "" && currentOK && currentHead != "" && recordedHead != currentHead
		if !staleHead {
			s.writeError(w, r, http.StatusUnprocessableEntity, "retry_not_applicable",
				"a verdict was already recorded for this acceptance stage and it corresponds to the run's current head; verdict-ful routing belongs to the deterministic triage, not a re-open",
				map[string]any{"stage_id": stage.ID.String()})
			return
		}
		// Stale-head verdict: the recorded head differs from the run's current
		// head, so a fix-up push invalidated it. Fall through to the re-open.
	}

	// (ii.b) Skip-marker backstop (E38.3 / #1877). A no-verdict acceptance stage
	// that carries a stage-scoped acceptance_skipped_out_of_scope marker was
	// auto-terminated because the approved plan declared verification.out_of_scope
	// with zero acceptance_criteria — there is no observable criterion for a
	// re-run to validate, so a reopen would re-fire the same skip forever. Refuse
	// with 422 retry_not_applicable; the run is already merge-eligible via the
	// gate's acceptanceGateSkippedOutOfScope state. A marker read error 500s like
	// the sibling verdict read above — never admit a reopen on unknown evidence.
	if !hasVerdict {
		skipped, serr := s.acceptanceStageSkippedOutOfScope(ctx, stage.RunID, stage.ID)
		if serr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list acceptance skip-marker audit entries failed",
				map[string]any{"error": serr.Error()})
			return
		}
		if skipped {
			s.writeError(w, r, http.StatusUnprocessableEntity, "retry_not_applicable",
				"the acceptance stage was auto-terminated because the approved plan declared verification.out_of_scope with zero acceptance_criteria (E38.3); a reopen would re-fire the same skip with no observable criterion to validate, and the run is already merge-eligible",
				map[string]any{"stage_id": stage.ID.String()})
			return
		}
	}

	// (iii) Re-open succeeded → pending via the class-2 verb. A refusal
	// (wrong type, not succeeded, terminal run) maps to 422.
	dec, err := run.ReopenAcceptanceStage(ctx, s.cfg.RunRepo, stage.ID)
	if err != nil {
		if errors.Is(err, run.ErrAcceptanceReopenNotApplicable) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "retry_not_applicable",
				err.Error(), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"reopen acceptance stage failed", map[string]any{"error": err.Error()})
		return
	}

	// (iv) Audit-first: record the reopen before the orchestrator handoff so
	// the intent is durable even if Advance below fails.
	s.writeAcceptanceReopenAudit(ctx, id, dec)

	// (v) Hand off to the orchestrator so it walks pending → dispatched and
	// rebuilds a fresh preview. WARN-on-error: the stage stays pending for a
	// manual re-fire, mirroring routeAcceptanceClass2 / the retry handler.
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, aerr := s.cfg.Orchestrator.Advance(ctx, stage.RunID); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"acceptance reopen: orchestrator advance failed",
				slog.String("run_id", stage.RunID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", aerr.Error()))
		}
		if updated, gerr := s.cfg.RunRepo.GetStage(ctx, stage.ID); gerr == nil {
			dec.Stage = updated
		}
	}

	s.notifyStatusUpdate(ctx, stage.RunID, "acceptance_reopened")

	s.writeJSON(w, r, http.StatusOK, toStageResponse(dec.Stage))
}

// latestAcceptanceVerdictHead returns the head_sha payload field of the
// highest-sequence acceptance_outcome_recorded entry scoped to stageID, and
// whether any verdict exists for the stage (#1682 Option C). A verdict entry
// with no head_sha (a pre-#1682 record) yields ("", true) — the caller then
// fails closed to the 422 because an empty recorded head cannot prove staleness.
func latestAcceptanceVerdictHead(entries []*audit.Entry, stageID uuid.UUID) (string, bool) {
	var (
		seq   int64
		head  string
		found bool
	)
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		if !found || e.Sequence > seq {
			var p struct {
				HeadSHA string `json:"head_sha"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			head = p.HeadSHA
			seq = e.Sequence
			found = true
		}
	}
	return head, found
}

// writeAcceptanceReopenAudit appends the acceptance_reopened chained entry for
// an operator-gated re-open of a settled-outcome-unknown acceptance stage
// (#1567). Records the actor, the prior state (always succeeded on the
// admitted path), and the reason. Best-effort: the transition is already
// committed, so a failure here logs but doesn't unwind.
func (s *Server) writeAcceptanceReopenAudit(ctx context.Context, id Identity, dec *run.AcceptanceReopenDecision) {
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	payload, _ := json.Marshal(map[string]any{
		"stage_id":    dec.Stage.ID.String(),
		"prior_state": string(dec.PriorState),
		"reason":      "acceptance stage settled succeeded with no recorded outcome; operator re-opened for a re-run",
	})

	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryAcceptanceReopened,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for acceptance reopen",
			"run_id", dec.Stage.RunID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
	}
}

// retryActionParams carries the resolved inputs for retryStageAs. The HTTP
// handler computes them from the request body + identity; the in-process
// campaign auto-driver (E25.6) supplies them directly.
type retryActionParams struct {
	StageID        uuid.UUID
	Override       bool
	OverrideReason string
	DelegatedRule  string
}

// retryStageAs performs the gate-action core of POST /v0/stages/{id}/retry
// under the given identity: run.RetryStage, the audit write (ordinary
// stage_retried receipt or the distinct stage_override_retried entry), the
// failed→running run un-terminal, the orchestrator handoff, the drive
// stamp, and the status notify. It is identity-parameterised so the HTTP
// handler and the in-process campaign auto-driver (E25.6 / ADR-047) drive
// the identical path and stamp identical audit. run.RetryStage's sentinel
// errors are returned verbatim for the caller to map; every post-transition
// step is best-effort exactly as in the prior inline handler.
func (s *Server) retryStageAs(ctx context.Context, id Identity, p retryActionParams) (*run.Stage, error) {
	// Enforce the retry gate's write scope on the acting identity (write:stages
	// OR write:retries, matching the handler's inline check). A no-op on the HTTP
	// path that already gated; the authz check for the in-process campaign
	// auto-driver, which reaches this method directly (#1445).
	if !identityHasGateScope(id, "write:stages", "write:retries") {
		return nil, &gateActionScopeError{scope: "write:stages or write:retries"}
	}
	dec, err := run.RetryStage(ctx, s.cfg.RunRepo, p.StageID, run.RetryOptions{OverrideB: p.Override})
	if err != nil {
		return nil, err
	}

	// Best-effort fetch run for budget info in the audit receipt.
	// A failure here is logged and the audit receipt omits the budget
	// fields rather than failing the request.
	var runRow *run.Run
	if fetched, fetchErr := s.cfg.RunRepo.GetRun(ctx, dec.Stage.RunID); fetchErr == nil {
		runRow = fetched
	} else {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"get run for retry receipt failed",
			slog.String("run_id", dec.Stage.RunID.String()),
			slog.String("error", fetchErr.Error()))
	}

	// Audit first so the retry intent is recorded even if the
	// orchestrator handoff below fails. Same posture as the
	// approvals handler (E7.4 / approvals.go). A category-B override
	// gets the distinct stage_override_retried entry (who/why) instead
	// of the ordinary stage_retried receipt.
	if dec.Overridden {
		s.writeOverrideRetryAudit(ctx, id, dec, p.OverrideReason)
	} else {
		s.writeRetryAudit(ctx, id, dec, runRow, p.DelegatedRule)
	}

	// Un-terminal the run (failed → running) before the orchestrator
	// handoff. This is MANDATORY, not cosmetic: orchestrator.Advance
	// returns OutcomeNoOp without acting when run.State.IsTerminal()
	// (orchestrator.go early-return after GetRun), so re-opening only
	// the stage would strand the run with the re-run's work landed and
	// the next gate never opening — the #798 orphan. Mirrors #698's
	// RedriveChild, which performs the identical failed → running reopen
	// via the same RetryRun primitive (the runRetryTransitions table in
	// transition.go permits only failed → running). Gated on
	// State == failed so it is inert when no run row is resolvable
	// (runRow nil) and a no-op-by-rejection is avoided for an
	// already-running run (running → running is not in the retry table).
	// Applied on the retryable path generally (not only the pending
	// branch) so the D-timeout → awaiting_approval case is also
	// un-terminalled and a later approve's Advance is not a no-op.
	// Best-effort: the stage transition already committed inside
	// run.RetryStage, so a RetryRun error here logs and does not fail
	// the request — same audit-first posture as the Advance handoff.
	if runRow != nil && runRow.State == run.StateFailed {
		if _, err := s.cfg.RunRepo.RetryRun(ctx, dec.Stage.RunID, run.StateRunning); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"reopen run failed → running for retry failed",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	// Capture the re-open shape BEFORE the post-Advance GetStage re-fetch
	// below overwrites dec.Stage.State: the drive recording keys off
	// whether the retry re-opened the stage to pending (only the
	// retryable A/C paths do — D-timeout retries land at awaiting_approval),
	// independent of whatever state the orchestrator advance then reached.
	reopenedToPending := dec.Stage.State == run.StageStatePending
	retriedStageType := dec.Stage.Type
	retriedStageID := dec.Stage.ID
	retriedRunID := dec.Stage.RunID

	// A/C retries land the stage in pending; hand off to the
	// orchestrator to walk pending → dispatched and fire
	// workflow_dispatch. D-timeout retries land at
	// awaiting_approval and don't need the orchestrator (no
	// dispatch to fire — the gate just re-opens).
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(ctx, dec.Stage.RunID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"orchestrator advance failed for retry",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", err.Error()))
			// Don't fail the request: the audit row recorded the
			// retry intent and the stage is in pending. Operator
			// can re-fire Advance manually. Re-fetch the stage so
			// the response reflects whatever state the orchestrator
			// did manage to reach before failing.
		}
		// Re-fetch the stage post-orchestrator so the response
		// reflects dispatched / awaiting_approval, not pending.
		if updated, err := s.cfg.RunRepo.GetStage(ctx, dec.Stage.ID); err == nil {
			dec.Stage = updated
		}
	}

	// Drive (#1271): a retry that re-opened the stage to pending is the
	// retry_reopen transition point — the orchestrator handoff above IS
	// the auto-advance (workflow_dispatch for runner_kind github_actions),
	// so stamp the run_auto_advanced entry that surfaces the required next
	// action on the authoritative REST run resource; runner_kind local
	// parks with a host-side run_<stage>_stage next action instead
	// (ADR-024: the runner is host-spawned, the backend has no execution
	// channel to it). Mirrors recordDriveReviseReplan; gated on the
	// re-opened-to-pending shape captured before the re-fetch so it fires
	// only for the retryable A/C re-opens, never the D-timeout
	// awaiting_approval re-open.
	if reopenedToPending {
		s.recordDriveRetryStage(ctx, retriedStageType, retriedStageID, retriedRunID)
	}

	// Sticky status comment (E20.4 / #330). A retry flips a failed
	// stage back to pending / dispatched / awaiting_approval; the
	// status comment should reflect the new shape.
	s.notifyStatusUpdate(ctx, dec.Stage.RunID, "stage_retry")

	return dec.Stage, nil
}

// recordDriveRetryStage stamps the drive engine's retry_reopen rule
// (#1271) after a retry re-opens a failed stage to pending. No-ops for
// non-drive runs, when no engine is wired, or on a run read failure
// (best-effort: the retry already landed; a missing stamp degrades
// attribution, never the run). For runner_kind github_actions the entry
// records the advance the orchestrator's workflow_dispatch fired; for
// runner_kind local it records the park (Parked=true) with the
// run_<stage>_stage next action that surfaces on the REST run resource and
// MCP get_run_status. A stage type with no host-side next action (the
// defensive EvaluateRetryReopen nil arm — review/D-timeout never reach the
// pending re-open) records nothing. The entry is keyed to the re-opened
// stage.
func (s *Server) recordDriveRetryStage(ctx context.Context, stageType run.StageType, stageID, runID uuid.UUID) {
	if s.drive == nil || s.cfg.RunRepo == nil {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil || !runRow.Drive {
		return
	}
	out := drive.EvaluateRetryReopen(runRow.RunnerKind, stageType)
	adv := drive.Advance{
		Rule: drive.RuleRetryReopen,
		From: string(stageType) + ":retried",
	}
	if out.Advance {
		adv.To = string(stageType) + ":dispatched"
		adv.Event = "failed " + string(stageType) + " stage retried; orchestrator re-dispatched the stage via workflow_dispatch"
	} else {
		if out.NextAction == nil {
			// Unsupported stage type for a pending re-open (defensive): emit
			// no entry rather than a bogus next action.
			return
		}
		adv.To = string(stageType) + ":pending"
		adv.Event = "failed " + string(stageType) + " stage retried; runner_kind local parks for a host-side re-dispatch"
		adv.Parked = true
		adv.NextAction = out.NextAction
	}
	s.drive.Record(ctx, runID, &stageID, adv)
}

// writeRetryAudit appends a stage_retried entry capturing the
// prior failure category + reason, the retry receipt fields, and
// the actor that triggered the retry. When delegatedRule is non-empty
// the retry landed via the ADR-040 delegated path (#1026) and the
// payload records `delegated: "<rule>"`. Best-effort — the transition
// is already committed, so a failure here logs but doesn't unwind.
func (s *Server) writeRetryAudit(ctx context.Context, id Identity, dec *run.RetryDecision, runRow *run.Run, delegatedRule string) {
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	ordinal := dec.Stage.SelfRetryCount

	fields := map[string]any{
		"stage_id":            dec.Stage.ID.String(),
		"prior_category":      string(dec.PriorCategory),
		"prior_reason":        dec.PriorReason,
		"prior_failure_class": dec.PriorCategory.Description(),
		"retry_ordinal":       ordinal,
		"admissibility_reason": fmt.Sprintf("category %s (%s); retry %d; via %s",
			string(dec.PriorCategory),
			dec.PriorCategory.Description(),
			ordinal,
			scopeUsed(id)),
	}
	if runRow != nil {
		remaining := runRow.MaxRetriesSnapshot - ordinal
		if remaining < 0 {
			remaining = 0
		}
		fields["remaining_budget"] = remaining
	}
	if delegatedRule != "" {
		fields["delegated"] = delegatedRule
	}

	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageRetried,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for retry",
			"run_id", dec.Stage.RunID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
	}
}

// writeOverrideRetryAudit appends the distinct stage_override_retried
// entry for an audited category-B override (#698). It records the
// actor, the prior category/reason (always B here), and the operator's
// required justification. The framing is explicit per the approval
// condition: the override re-opens the stage to pending so the agent
// re-runs and the policy gate re-evaluates the new diff — it does NOT
// accept the B-violating diff or bypass the gate. Best-effort: the
// transition is already committed, so a failure here logs but doesn't
// unwind.
func (s *Server) writeOverrideRetryAudit(ctx context.Context, id Identity, dec *run.RetryDecision, reason string) {
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	fields := map[string]any{
		"stage_id":            dec.Stage.ID.String(),
		"prior_category":      string(dec.PriorCategory),
		"prior_reason":        dec.PriorReason,
		"prior_failure_class": dec.PriorCategory.Description(),
		"override_reason":     reason,
		"retry_ordinal":       dec.Stage.SelfRetryCount,
		// Framing (approval condition): the override does not bypass the
		// gate — it re-opens the B stage to pending for a fresh run, and
		// the policy gate re-evaluates the new diff.
		"override_effect": "re-opened to pending for re-run; the policy gate re-evaluates the new diff and is not bypassed",
		"admissibility_reason": fmt.Sprintf("category %s (%s) override; re-run + gate re-eval; via %s",
			string(dec.PriorCategory),
			dec.PriorCategory.Description(),
			scopeUsed(id)),
	}

	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageOverrideRetried,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for override retry",
			"run_id", dec.Stage.RunID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
	}
}

// scopeUsed returns the scope string that authorized the retry for
// inclusion in the admissibility_reason receipt field.
func scopeUsed(id Identity) string {
	if hasScope(id, "write:retries") {
		return "write:retries"
	}
	if hasScope(id, "write:stages") {
		return "write:stages"
	}
	return "session"
}
