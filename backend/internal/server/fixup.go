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
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryStageFixupTriggered is the audit-log category for the entry
// the fix-up handler writes when an implement stage is re-opened to
// route advisory implement-review concerns back to the agent (E22.X /
// #762). The payload carries the operator-selected concern indices and
// the resolved concern objects so the prompt renderer can deliver them
// as binding instructions, plus the bounded-pass receipt fields. This
// entry IS the durable record of the fix-up bound: the handler counts
// prior entries of this category for the stage to enforce the
// configured max-passes ceiling — there is no dedicated column.
const CategoryStageFixupTriggered = "stage_fixup_triggered"

// CategoryStageFixupRecovered is the audit-log category for the entry
// maybeRecoverFixupFailure writes when a FAILED fix-up re-dispatch is
// recovered back to the run's pre-fix-up review gate (E22.X / #788).
// A fix-up re-opens an implement stage from a HEALTHY gate (the PR is
// open and mergeable); if the re-dispatched implement run fails, the
// stage would land terminal `failed` and destroy the intact original
// work. This entry records the restoration — the implement stage's
// restored prior state, the re-parked review stage id (when any), and
// the source failure category/reason the re-dispatch failed with. It is
// a system-actor, internal audit kind — NOT an issue-comment surface.
const CategoryStageFixupRecovered = "stage_fixup_recovered"

// defaultMaxFixupPasses bounds the number of fix-up passes a single
// implement stage may consume. Default 1 — a fix-up is a bounded,
// operator-gated single pass, never an unbounded auto-loop. Making
// this spec-configurable is deferred (it needs a workflow-spec field;
// out of scope for the trigger surface).
const defaultMaxFixupPasses = 1

// defaultFixupCeiling is the absolute hard cap on total fix-up passes a
// single implement stage may consume, INCLUDING any operator-forced
// override pass (#860). The normal budget is defaultMaxFixupPasses (1);
// once spent, an operator may grant ONE additional pass via
// force_additional_pass, but never past this ceiling. At the ceiling the
// handler refuses with the distinct fixup_ceiling_reached error (a hard
// stop: merge-with-follow-up or a fresh run), not fixup_budget_exhausted.
const defaultFixupCeiling = 3

// fixupRequest is the JSON body of POST /v0/stages/{stage_id}/fixup.
// ConcernIDs is the PRIMARY addressing scheme (#964): the stable
// concern UUIDs (surfaced by GET /v0/runs/{run_id}'s concerns block) to
// route back to the agent. Concerns (positional indices into the
// stage's flattened resolved concern set) is DEPRECATED — with multiple
// heterogeneous review entries per stage the flattened index is
// ambiguous (the run-73456dc8 mis-route) — and remains only as a
// fallback when ConcernIDs is absent; supplying both is rejected.
// Reason is an optional operator note recorded on the audit entry.
// AllowCreate declares net-new files this fix-up pass will create
// (#823); the paths are folded into the effective scope.files for THAT
// dispatch only so the runner's #818 created-out-of-scope gate stages
// them rather than failing category-B. Any created file NOT declared
// here still trips the gate.
type fixupRequest struct {
	ConcernIDs  []string `json:"concern_ids"`
	Concerns    []int    `json:"concerns"`
	Reason      string   `json:"reason"`
	AllowCreate []string `json:"allow_create"`
	// ForceAdditionalPass is the bounded operator override (#860): when
	// true it grants ONE fix-up pass beyond the normal budget
	// (defaultMaxFixupPasses), hard-capped at defaultFixupCeiling total
	// passes per stage. The forced pass is audited (a `forced` flag plus
	// the operator reason). Default false preserves the prior behaviour.
	ForceAdditionalPass bool `json:"force_additional_pass"`
	// Delegated opts the fix-up into the ADR-040 delegated-action path
	// (#1026): checkDelegation re-evaluates the operator_agent
	// may_route_fixup condition server-side at action time — 403
	// delegation_not_configured / delegation_condition_unmet on refusal,
	// `delegated: "<rule>"` on the stage_fixup_triggered payload when
	// met. Absent → behavior byte-identical to today.
	Delegated bool `json:"delegated"`
	// ImplementModel is the optional operator/driver model override for
	// THIS fix-up pass (#1164, the #1013 operator rung applied to the
	// fix-up path). Empty == today's behavior: the fix-up inherits the
	// run's already-resolved implement model (byte-identical default).
	// A non-empty value is validated against the deployment's per-adapter
	// allow-list exactly as the plan gate validates it (422
	// fixup_invalid_model on reject). The effective model — override or
	// inherited — is pinned on the stage_fixup_triggered audit entry at
	// trigger time and read back when the runner fetches the fix-up prompt.
	ImplementModel string `json:"implement_model"`
}

// validateAllowCreate normalizes and validates the fix-up allow-create
// paths (#823). Each entry is trimmed; empty/whitespace-only, absolute,
// and ".."-containing entries are rejected so the allow-list stays
// repo-relative and contained (it cannot widen scope outside the tree).
// Returns the trimmed paths on success, or a (field, message) describing
// the first bad entry for a 400 validation_failed envelope.
func validateAllowCreate(paths []string) ([]string, string, string) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			return nil, "allow_create", "allow_create entries must be non-empty repo-relative paths"
		}
		if strings.HasPrefix(trimmed, "/") {
			return nil, "allow_create", fmt.Sprintf("allow_create entry %q must be repo-relative, not absolute", trimmed)
		}
		if strings.Contains(trimmed, "..") {
			return nil, "allow_create", fmt.Sprintf("allow_create entry %q must not contain '..'", trimmed)
		}
		out = append(out, trimmed)
	}
	return out, "", ""
}

// handleFixupStage implements POST /v0/stages/{stage_id}/fixup.
//
// The implement-review fix-up (E22.X / #762, #780) routes one or more
// advisory implement-review concerns (ADR-027 approve_with_concerns)
// back to the implement agent for a bounded, operator-gated fix-up
// pass, instead of the operator hand-editing the PR branch. It re-opens
// the implement stage to pending and hands off to the orchestrator,
// which re-dispatches it. Two flows are admitted by run.FixupStage:
//
//   - commit-yourself: the implement stage is its own gate
//     (awaiting_approval → pending);
//   - push_and_open_pr (#780): the implement stage SUCCEEDED (PR opened)
//     and the human gate is a SEPARATE review stage at awaiting_approval.
//     The implement stage re-opens succeeded → pending AND the review
//     stage is re-parked awaiting_approval → pending so the re-dispatched
//     implement flows back into a fresh review. The re-parked review
//     stage id is recorded on the audit entry (reparked_review_stage_id).
//
// The selected concerns are delivered to the agent as binding
// instructions by the prompt renderer (reading them back from the audit
// entry this handler writes).
//
// Distinct from POST /v0/stages/{stage_id}/retry: retry re-opens a
// FAILED stage and regenerates a fresh diff; fix-up re-opens a HEALTHY
// gate, commits onto the SAME PR branch, and is bounded
// (defaultMaxFixupPasses) by counting prior stage_fixup_triggered
// audit entries.
func (s *Server) handleFixupStage(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:stages") && !hasScope(id, "write:fixups") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages or write:fixups",
			map[string]any{"required_scope": "write:stages or write:fixups"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "fixup_unconfigured",
			"fixup endpoint requires run + audit repositories", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	var reqBody fixupRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {concerns, reason}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if len(reqBody.ConcernIDs) > 0 && len(reqBody.Concerns) > 0 {
		// Mixed addressing is rejected outright: the two schemes can
		// disagree about which concern they name, and silently preferring
		// one would reintroduce the ambiguity stable IDs exist to remove.
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"supply concern_ids (stable concern UUIDs) OR the deprecated positional concerns indices, not both",
			map[string]any{"field": "concern_ids"})
		return
	}
	if len(reqBody.ConcernIDs) == 0 && len(reqBody.Concerns) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concern_ids must select at least one recorded implement-review concern (stable UUIDs from the run's concerns block; the positional concerns field is a deprecated fallback)",
			map[string]any{"field": "concern_ids"})
		return
	}
	concernIDs := make([]uuid.UUID, 0, len(reqBody.ConcernIDs))
	for _, raw := range reqBody.ConcernIDs {
		cid, perr := uuid.Parse(raw)
		if perr != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				fmt.Sprintf("concern_ids entry %q is not a valid UUID", raw),
				map[string]any{"field": "concern_ids", "got": raw})
			return
		}
		concernIDs = append(concernIDs, cid)
	}

	// Validate the optional allow-create paths (#823) before any state
	// change: trim, reject empty/absolute/".."-containing entries.
	allowCreate, badField, badMsg := validateAllowCreate(reqBody.AllowCreate)
	if badField != "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			badMsg, map[string]any{"field": badField})
		return
	}

	// Pre-fetch the stage to get the RunID for the subject-binding guard
	// and the audit lookups below.
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

	// Subject-binding guard: an MCP run-bound token may only fix up
	// stages within its own run. Subject format is "mcp:run:<uuid>"
	// (set by bearerAuth middleware). Mirrors the retry handler.
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		runIDStr := strings.TrimPrefix(id.Subject, "mcp:run:")
		subjectRunID, parseErr := uuid.Parse(runIDStr)
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != stage.RunID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_fixup",
				"mcp token may only fix up stages within its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"stage_run_id": stage.RunID.String(),
				})
			return
		}
	}

	// Delegated-action enforcement (ADR-040 / #1026): a delegated:true
	// fix-up must hold the may_route_fixup condition against CURRENT run
	// state, re-evaluated server-side before any state change.
	var delegatedRule string
	if reqBody.Delegated {
		rule, ok := s.checkDelegation(w, r, stage.RunID, delegation.ActionRouteFixup)
		if !ok {
			return
		}
		delegatedRule = rule
	}

	// Resolve the selected concerns. The primary path addresses them by
	// stable ID against the durable concern store (#964); the deprecated
	// fallback resolves positional indices against the flattened
	// implement_reviewed audit-entry concern set.
	var selected []planreview.Concern
	if len(concernIDs) > 0 {
		if s.cfg.ConcernRepo == nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "fixup_unconfigured",
				"concern_ids addressing requires a configured concern repository; use the deprecated positional concerns field or configure the store",
				nil)
			return
		}
		rows, rerr := s.resolveConcernsByID(r.Context(), stage.RunID, stageID, concernIDs)
		if rerr != nil {
			var bad *concernSelectionError
			if errors.As(rerr, &bad) {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					bad.Error(), map[string]any{"field": "concern_ids"})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resolve concern_ids failed", map[string]any{"error": rerr.Error()})
			return
		}
		selected = rows
	} else {
		// DEPRECATED positional-index path. Flattened across every
		// reviewer entry, so with multiple heterogeneous reviews per
		// stage the index is ambiguous — prefer concern_ids.
		concerns, err := s.resolveImplementConcerns(r.Context(), stage.RunID, stageID)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resolve implement-review concerns failed", map[string]any{"error": err.Error()})
			return
		}
		if len(concerns) == 0 {
			s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_not_applicable",
				"stage has no recorded approve_with_concerns implement-review concerns to route back to the agent",
				nil)
			return
		}

		// Validate the selected indices against the resolved set and collect
		// the selected concern objects (deduped, in selection order).
		selected, err = selectConcerns(concerns, reqBody.Concerns)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(),
				map[string]any{"field": "concerns", "available": len(concerns)})
			return
		}
	}

	// Count prior fix-up passes for this stage to enforce the bound.
	priorPasses, err := s.countFixupPasses(r.Context(), stage.RunID, stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count prior fix-up passes failed", map[string]any{"error": err.Error()})
		return
	}

	// No-change refund (#967): a fix-up pass that produced no commit
	// (fixup_no_changes audit entry, #856) is refunded against the NORMAL
	// budget — the pass consumed a stage_fixup_triggered entry but changed
	// nothing on the PR branch. Implemented by widening MaxPasses by the
	// refunded count, which is equivalent to subtracting the refunds from
	// the budget comparison (raw >= max+refunded ⟺ raw-refunded >= max)
	// while HardCeiling keeps counting RAW triggered passes, so the
	// absolute 3-pass cap is unaffected and a pathologically no-op'ing
	// agent is still hard-stopped.
	refundedPasses, err := s.countFixupNoChangeRefunds(r.Context(), stage.RunID, stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count refunded fix-up passes failed", map[string]any{"error": err.Error()})
		return
	}
	if refundedPasses > priorPasses {
		// Defensive clamp: a refund can never exceed the passes actually
		// triggered (would widen the budget past the configured max).
		refundedPasses = priorPasses
	}

	// Fix-up model resolution + gate (#1164). Resolve the model this pass
	// will run under — the operator's implement_model override when supplied,
	// else the run's already-resolved implement model (byte-identical default)
	// — and validate it against the run adapter's allow-list BEFORE the
	// transition, so a disallowed override is refused (422 fixup_invalid_model)
	// with NO state change and NO audit entry. The resolved model is pinned on
	// the stage_fixup_triggered entry below so the prompt-fetch read-back rides
	// the live #1013 implement_model wire channel. Fail-OPEN on a run read
	// failure, mirroring checkPlanModelAllowed: pin the operator override (if
	// any) so the audit still records intent, but skip allow-list validation
	// (no adapter without the run) and let the transition below own the
	// applicability verdict.
	// pinModel carries the resolved fix-up model to writeFixupAudit when — and
	// ONLY when — a model was actually resolved. A nil pinModel means "do NOT
	// pin": writeFixupAudit omits the fixup_model key so the prompt-fetch
	// read-back (fixupResolvedModelFromAudit) returns ok=false and falls
	// through to live resolution. The sole nil case is a run-read failure with
	// no operator override: pinning the zero ResolvedModel would write a
	// present-but-empty fixup_model, which the PRESENCE-based read-back honors
	// as a deliberate empty-ladder pin — forcing the fix-up to spawn with an
	// EMPTY model instead of the run's already-resolved implement model. That
	// would violate the byte-identical default, so we leave it unpinned.
	var pinModel *ResolvedModel
	if runRow, runErr := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID); runErr == nil {
		rm := s.resolveFixupImplementModel(r.Context(), runRow, reqBody.ImplementModel)
		if !s.checkFixupModelAllowed(w, r, stage, runRow, rm) {
			return
		}
		pinModel = &rm
	} else {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "fixup model gate: get run failed; proceeding fail-open",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", runErr.Error()))
		// Fail-open pins the operator override (if any) so the audit records
		// intent; allow-list validation is intentionally skipped (no adapter
		// without the run). With NO override there is nothing to pin — leave
		// pinModel nil so the read-back re-resolves the run's implement model
		// rather than pinning an empty one over it.
		if ov := strings.TrimSpace(reqBody.ImplementModel); ov != "" {
			pinModel = &ResolvedModel{Value: ov, Source: ModelSourceOperator}
		}
	}

	dec, err := run.FixupStage(r.Context(), s.cfg.RunRepo, stageID, run.FixupOptions{
		PriorPassCount:      priorPasses,
		MaxPasses:           defaultMaxFixupPasses + refundedPasses,
		ForceAdditionalPass: reqBody.ForceAdditionalPass,
		HardCeiling:         defaultFixupCeiling,
	})
	if err != nil {
		switch {
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", nil)
			return
		case errors.Is(err, run.ErrFixupCeilingReached):
			// Placed BEFORE the budget-exhausted arm so the distinct
			// hard-stop error is not masked (the override cannot push past
			// this — #860).
			s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_ceiling_reached",
				err.Error(), map[string]any{"ceiling": defaultFixupCeiling, "used": priorPasses})
			return
		case errors.Is(err, run.ErrFixupBudgetExhausted):
			s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_budget_exhausted",
				err.Error(), map[string]any{"max_passes": defaultMaxFixupPasses, "used": priorPasses, "refunded_passes": refundedPasses})
			return
		case errors.Is(err, run.ErrFixupNotApplicable):
			s.writeError(w, r, http.StatusUnprocessableEntity, "fixup_not_applicable",
				err.Error(), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"fixup failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit first so the fix-up intent (and the selected concerns the
	// prompt renderer reads back) is recorded even if the orchestrator
	// handoff below fails. Same posture as the retry handler.
	s.writeFixupAudit(r, dec, selected, concernIDs, reqBody.Concerns, reqBody.Reason, allowCreate, priorPasses, refundedPasses, delegatedRule, pinModel)

	// Mark the routed concerns addressed_pending in the durable store
	// (#964), recording the operator's reason. AFTER the audit append so
	// the trigger entry is the durable record; best-effort like the
	// append — a failure warn-logs and never unwinds the committed
	// transition.
	if len(concernIDs) > 0 && s.cfg.ConcernRepo != nil {
		if merr := s.cfg.ConcernRepo.MarkAddressedPending(r.Context(), concernIDs, reqBody.Reason); merr != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"fixup: mark concerns addressed_pending failed",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", merr.Error()))
		}
	}

	// The fix-up re-open lands the stage in pending; hand off to the
	// orchestrator to walk pending → dispatched and fire
	// workflow_dispatch. Orchestrator failures are logged but don't fail
	// the request: the audit row recorded the intent and the stage is in
	// pending, so an operator can re-fire Advance manually.
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), dec.Stage.RunID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"orchestrator advance failed for fixup",
				slog.String("run_id", dec.Stage.RunID.String()),
				slog.String("stage_id", dec.Stage.ID.String()),
				slog.String("error", err.Error()))
		}
		if updated, err := s.cfg.RunRepo.GetStage(r.Context(), dec.Stage.ID); err == nil {
			dec.Stage = updated
		}
	}

	// Drive (#1023): the push_and_open_pr re-park (review gate
	// awaiting_approval → pending, #780) is the fixup_rereview_repark
	// transition point on a drive-enabled run — stamp it so the
	// re-review round the re-park triggers is attributable to a named
	// rule. Keyed to the re-parked REVIEW stage (the stage whose state
	// changed), not the implement stage being re-opened.
	if dec.ReparkedReview != nil {
		s.recordDriveFixupRepark(r.Context(), dec)
	}

	// Sticky status comment (E20.4 / #330): a fix-up flips the stage back
	// to pending / dispatched; the status comment should reflect that.
	s.notifyStatusUpdate(r.Context(), dec.Stage.RunID, "stage_fixup")

	s.writeJSON(w, r, http.StatusOK, toStageResponse(dec.Stage))
}

// recordDriveFixupRepark stamps the drive engine's
// fixup_rereview_repark rule (#1023) after a fix-up re-parked the
// review gate. No-ops for non-drive runs, when no engine is wired, or
// on a run read failure — best-effort like every drive stamp: the
// re-park already happened, a missing entry degrades attribution only.
func (s *Server) recordDriveFixupRepark(ctx context.Context, dec *run.FixupDecision) {
	if s.drive == nil || s.cfg.RunRepo == nil || dec.ReparkedReview == nil {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, dec.Stage.RunID)
	if err != nil || !runRow.Drive {
		return
	}
	s.drive.Record(ctx, dec.Stage.RunID, &dec.ReparkedReview.ID, drive.Advance{
		Rule:  drive.RuleFixupRereviewRepark,
		From:  "review:awaiting_approval",
		To:    "review:pending",
		Event: fmt.Sprintf("fix-up re-opened implement stage %s; review gate re-parked for a fresh round", dec.Stage.ID),
	})
}

// concernSelectionError marks a concern_ids selection problem the
// handler maps to 400 validation_failed (unknown ID, a concern from a
// different run/stage, a plan-stage concern, or a non-open state) as
// opposed to an infrastructure failure (500).
type concernSelectionError struct{ msg string }

func (e *concernSelectionError) Error() string { return e.msg }

// resolveConcernsByID resolves stable concern UUIDs against the durable
// concern store, scoped to the implement stage being fixed up (#964).
// Every ID must name an implement-stage concern of THIS stage in an
// open state — a plan-stage concern ID (surfaced by the same run-status
// block) is rejected explicitly so it can never route into an implement
// fix-up. Returns the selected concerns in selection order as the
// planreview shape the audit payload and prompt renderer consume.
func (s *Server) resolveConcernsByID(ctx context.Context, runID, stageID uuid.UUID, ids []uuid.UUID) ([]planreview.Concern, error) {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return nil, &concernSelectionError{msg: fmt.Sprintf("concern_id %s selected more than once", id)}
		}
		seen[id] = struct{}{}
	}
	rows, err := s.cfg.ConcernRepo.GetByIDs(ctx, ids)
	if err != nil {
		if errors.Is(err, concern.ErrNotFound) {
			return nil, &concernSelectionError{msg: fmt.Sprintf("unknown concern_id: %s", err.Error())}
		}
		return nil, fmt.Errorf("get concerns by id: %w", err)
	}
	out := make([]planreview.Concern, 0, len(rows))
	for _, c := range rows {
		if c.StageKind != concern.StageKindImplement {
			return nil, &concernSelectionError{msg: fmt.Sprintf(
				"concern_id %s is a %s-stage concern; only implement-stage concerns can be routed into an implement fix-up", c.ID, c.StageKind)}
		}
		if c.RunID != runID || c.StageID != stageID {
			return nil, &concernSelectionError{msg: fmt.Sprintf(
				"concern_id %s belongs to a different run/stage than the fix-up target", c.ID)}
		}
		if !c.State.IsOpen() {
			return nil, &concernSelectionError{msg: fmt.Sprintf(
				"concern_id %s is not open (state %s); only raised/addressed_pending/reopened concerns can be routed", c.ID, c.State)}
		}
		out = append(out, planreview.Concern{
			Severity: planreview.ConcernSeverity(c.Severity),
			Category: c.Category,
			Note:     c.Note,
			// Carry the reviewer-emitted suggested_patch (#1165 slice 1) through
			// to the routed concern set so the trigger audit's `concerns` field
			// retains it. The prompt-serve path (resolveFixupApplyPatches) reads
			// it back and, when EVERY routed concern carries one, serves the
			// deterministic apply-list to the runner; without this copy the
			// store's patch would be dropped here and the apply path could never
			// engage for concern_ids-addressed fix-ups.
			SuggestedPatch: c.SuggestedPatch,
		})
	}
	return out, nil
}

// resolveImplementConcerns gathers the stage's recorded implement-
// review concerns from implement_reviewed audit entries with an
// approve_with_concerns verdict, in append order, flattened across
// every reviewer. This is the ordered set the operator's selected
// indices address.
func (s *Server) resolveImplementConcerns(ctx context.Context, runID, stageID uuid.UUID) ([]planreview.Concern, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "implement_reviewed")
	if err != nil {
		return nil, fmt.Errorf("list implement_reviewed audit entries: %w", err)
	}
	var concerns []planreview.Concern
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var p planreview.ImplementReviewedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			// Tolerate a malformed payload rather than failing the whole
			// resolve — skip the entry; the audit log is append-only and a
			// corrupt blob shouldn't wedge the fix-up surface.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"fixup: skipping malformed implement_reviewed payload",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
			continue
		}
		if p.Verdict != planreview.VerdictApproveWithConcerns {
			continue
		}
		concerns = append(concerns, p.Concerns...)
	}
	return concerns, nil
}

// countFixupPasses returns the number of prior stage_fixup_triggered
// audit entries recorded for the stage — the durable fix-up-pass
// counter the bound is enforced against. This RAW count always feeds
// the hard-ceiling check; the no-change refund (#967) never reduces it.
func (s *Server) countFixupPasses(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		return 0, fmt.Errorf("list %s audit entries: %w", CategoryStageFixupTriggered, err)
	}
	n := 0
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			n++
		}
	}
	return n, nil
}

// countFixupNoChangeRefunds returns the number of fixup_no_changes audit
// entries recorded for the stage (#856 report path, pullrequest.go) — the
// fix-up passes that produced no commit and are refunded against the
// NORMAL budget (#967). The report path's stage-keyed idempotency dedup
// admits at most one such entry per stage, so in practice this is 0 or 1.
func (s *Server) countFixupNoChangeRefunds(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, "fixup_no_changes")
	if err != nil {
		return 0, fmt.Errorf("list fixup_no_changes audit entries: %w", err)
	}
	n := 0
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			n++
		}
	}
	return n, nil
}

// selectConcerns validates the operator-selected indices against the
// resolved concern set and returns the selected concern objects in
// selection order, de-duplicated. An out-of-range or duplicate index
// is rejected so the prompt renderer never sees a phantom concern.
func selectConcerns(all []planreview.Concern, indices []int) ([]planreview.Concern, error) {
	seen := map[int]struct{}{}
	out := make([]planreview.Concern, 0, len(indices))
	for _, i := range indices {
		if i < 0 || i >= len(all) {
			return nil, fmt.Errorf("concern index %d out of range [0,%d)", i, len(all))
		}
		if _, dup := seen[i]; dup {
			return nil, fmt.Errorf("concern index %d selected more than once", i)
		}
		seen[i] = struct{}{}
		out = append(out, all[i])
	}
	return out, nil
}

// writeFixupAudit appends a stage_fixup_triggered entry capturing the
// routed stable concern IDs (#964, the primary addressing scheme) or
// the deprecated selected indices, the resolved concern objects (so the
// prompt renderer delivers them as binding instructions), the operator
// reason, the declared allow-create paths (#823, folded into the
// effective scope.files for the fix-up dispatch), and the bounded-pass
// receipt fields. When pinModel is non-nil it pins the resolved fix-up model
// (#1164) as fixup_model / fixup_model_source so the prompt-fetch read-back
// rides the #1013 implement_model wire deterministically; a nil pinModel —
// the run-read-failure / no-override fail-open path — OMITS those keys so the
// presence-based read-back falls through to live resolution rather than pinning
// an empty model over the run's already-resolved one. When delegatedRule is
// non-empty the fix-up landed via the ADR-040 delegated path (#1026) and
// the payload records `delegated: "<rule>"`. Best-effort: the transition is already
// committed, so a failure here logs but doesn't unwind.
func (s *Server) writeFixupAudit(r *http.Request, dec *run.FixupDecision, selected []planreview.Concern, concernIDs []uuid.UUID, indices []int, reason string, allowCreate []string, priorPasses, refundedPasses int, delegatedRule string, pinModel *ResolvedModel) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)

	passOrdinal := priorPasses + 1
	admissibilityReason := fmt.Sprintf("fix-up pass %d of %d; %d concern(s) routed back; via %s",
		passOrdinal, defaultMaxFixupPasses, len(selected), fixupScopeUsed(id))
	if refundedPasses > 0 {
		admissibilityReason += fmt.Sprintf("; %d no-change pass(es) refunded", refundedPasses)
	}
	if dec.Forced {
		// Durably record that this pass ran past the normal budget only
		// because the operator forced it (#860).
		admissibilityReason += "; operator-forced override"
	}
	routedIDs := make([]string, 0, len(concernIDs))
	for _, id := range concernIDs {
		routedIDs = append(routedIDs, id.String())
	}
	// Apply-eligibility provenance (#1165 slice 2): a fix-up is eligible for the
	// near-deterministic apply path ONLY when it routed at least one concern AND
	// every routed concern carries a non-empty suggested_patch. Recorded on the
	// trigger entry so an operator can see — at trigger time, before the runner
	// dispatch — whether this pass COULD collapse to a git-apply. The RUNTIME
	// outcome (applied | agent | apply_failed_fellback) rides the runner's
	// fixup_pushed report and lands on that entry (succeedFixupPushStage); this
	// boolean is the server-side eligibility half of the same provenance.
	applyEligible := fixupApplyEligible(selected)
	fields := map[string]any{
		"stage_id":             dec.Stage.ID.String(),
		"prior_state":          string(dec.PriorState),
		"concern_ids":          routedIDs,
		"selected_indices":     indices,
		"concerns":             selected,
		"reason":               reason,
		"allow_create":         allowCreate,
		"pass_ordinal":         passOrdinal,
		"max_passes":           defaultMaxFixupPasses,
		"hard_ceiling":         defaultFixupCeiling,
		"remaining_budget":     dec.RemainingBudget,
		"refunded_passes":      refundedPasses,
		"forced":               dec.Forced,
		"admissibility_reason": admissibilityReason,
		"apply_eligible":       applyEligible,
	}
	// Fix-up model pin (#1164): the source-tagged model this pass will run
	// under (operator override or the run's inherited resolution). Present
	// whenever a model was resolved — fixupResolvedModelFromAudit reads it back
	// at prompt-fetch by key PRESENCE, so an empty value (the empty-ladder
	// default spawn the gate deliberately resolved) is a deliberate pin, not
	// "absent". A nil pinModel — the run-read-failure / no-override fail-open
	// path — omits the keys so the read-back returns ok=false and falls through
	// to live resolution (the run's already-resolved implement model) rather
	// than forcing an empty model.
	if pinModel != nil {
		fields["fixup_model"] = pinModel.Value
		fields["fixup_model_source"] = string(pinModel.Source)
	}
	// push_and_open_pr flow (#780): record the review stage re-parked
	// alongside the implement re-open, so the audit trail captures the
	// full state change (and downstream tooling can correlate the gate).
	if dec.ReparkedReview != nil {
		fields["reparked_review_stage_id"] = dec.ReparkedReview.ID.String()
	}
	if delegatedRule != "" {
		fields["delegated"] = delegatedRule
	}

	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        dec.Stage.RunID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryStageFixupTriggered,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Error("audit append failed for fixup",
			"run_id", dec.Stage.RunID,
			"stage_id", dec.Stage.ID,
			"error", err.Error(),
		)
	}
}

// maybeRecoverFixupFailure detects a failed fix-up re-dispatch and, when
// it is one, restores the run to its pre-fix-up review gate instead of
// letting the run fail (E22.X / #788). It returns true ONLY when it
// recovered the run — the caller then SKIPS the run-failing orchestrator
// advance so the run stays `running` at its gate. On any miss (not an
// implement stage, no prior fix-up entry, payload unparseable, restore
// not applicable) it returns false and the normal failure path proceeds.
//
// The recovery signal is the durable stage_fixup_triggered audit entry:
// a fix-up re-opens an implement stage from a HEALTHY gate, so the only
// way the stage is `failed` AFTER such an entry exists is a re-dispatch
// failure. With the bounded budget plus operator-forced override (#860)
// up to defaultFixupCeiling such entries may exist per stage; the loop
// below keeps the LAST (most-recent) one via append order, so {an entry
// present + stage failed} still unambiguously identifies the latest
// fix-up re-dispatch failure (not an earlier unrelated failure).
//
// Best-effort and self-contained: it owns the recovery transition, the
// audit emission, and the status-comment refresh; a failure in any step
// returns false so the caller's normal Advance-to-failed path runs.
func (s *Server) maybeRecoverFixupFailure(ctx context.Context, runID, stageID uuid.UUID) bool {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return false
	}

	stage, err := s.cfg.RunRepo.GetStage(ctx, stageID)
	if err != nil || stage.Type != run.StageTypeImplement {
		return false
	}

	// Find the most-recent stage_fixup_triggered entry for this stage.
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryStageFixupTriggered)
	if err != nil {
		return false
	}
	var triggered *audit.Entry
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			triggered = e // entries are append-ordered; keep the last
		}
	}
	if triggered == nil {
		return false
	}

	var payload struct {
		PriorState            string `json:"prior_state"`
		ReparkedReviewStageID string `json:"reparked_review_stage_id"`
	}
	if err := json.Unmarshal(triggered.Payload, &payload); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup recovery: malformed stage_fixup_triggered payload — leaving failure path in force",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return false
	}

	var reviewStageID *uuid.UUID
	if payload.ReparkedReviewStageID != "" {
		rid, perr := uuid.Parse(payload.ReparkedReviewStageID)
		if perr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"fixup recovery: unparseable reparked_review_stage_id — leaving failure path in force",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", perr.Error()))
			return false
		}
		reviewStageID = &rid
	}

	recovery, err := run.RestoreFixupStage(ctx, s.cfg.RunRepo, stageID,
		run.StageState(payload.PriorState), reviewStageID)
	if err != nil {
		// ErrFixupRecoveryNotApplicable (the stage was not failed) is the
		// benign no-op; any other error means the restore didn't land.
		// Either way the normal failure path must proceed, so return false.
		if !errors.Is(err, run.ErrFixupRecoveryNotApplicable) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"fixup recovery: restore failed — leaving failure path in force",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
		}
		return false
	}

	s.writeFixupRecoveredAudit(ctx, runID, recovery)
	s.notifyStatusUpdate(ctx, runID, "stage_fixup_recovered")
	return true
}

// writeFixupRecoveredAudit appends a stage_fixup_recovered entry capturing
// the restored implement state, the re-parked review stage id (when any),
// and the source failure category/reason the fix-up re-dispatch failed
// with. Best-effort: the recovery transition is already committed, so a
// failure here logs but doesn't unwind.
func (s *Server) writeFixupRecoveredAudit(ctx context.Context, runID uuid.UUID, rec *run.FixupRecovery) {
	fields := map[string]any{
		"stage_id":       rec.Stage.ID.String(),
		"restored_state": string(rec.Stage.State),
	}
	if rec.RestoredReview != nil {
		fields["restored_review_stage_id"] = rec.RestoredReview.ID.String()
		fields["restored_review_state"] = string(rec.RestoredReview.State)
	}
	if rec.PriorFailureCategory != nil {
		fields["source_failure_category"] = string(*rec.PriorFailureCategory)
	}
	if rec.PriorFailureReason != nil {
		fields["source_failure_reason"] = *rec.PriorFailureReason
	}
	payload, _ := json.Marshal(fields)

	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &rec.Stage.ID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryStageFixupRecovered,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup recovery: append stage_fixup_recovered audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", rec.Stage.ID.String()),
			slog.String("error", err.Error()))
	}
}

// fixupApplyEligible reports whether a routed concern set qualifies for the
// near-deterministic apply path (#1165): at least one concern AND every routed
// concern carries a non-empty suggested_patch. A single patch-less concern
// (non-mechanical, or a reviewer that declined to emit a diff) makes the whole
// pass ineligible — the runner must re-derive the mixed change with the agent,
// so a partial apply is never attempted. Mirrors resolveFixupApplyPatches's
// all-or-nothing gate on the prompt-serve side.
func fixupApplyEligible(selected []planreview.Concern) bool {
	if len(selected) == 0 {
		return false
	}
	for _, c := range selected {
		if strings.TrimSpace(c.SuggestedPatch) == "" {
			return false
		}
	}
	return true
}

// Fix-up apply provenance values (#1165/#1213). The runner reports one of
// these on the fixup_pushed report's apply_path field to record whether the
// near-deterministic apply path resolved the fix-up or it fell back to the
// agent. They are the runtime sibling of fixupApplyEligible (the eligibility
// half of the same provenance): eligibility is decided at prompt-serve time,
// the realized path is reported at push time.
const (
	// fixupApplyPathApplied: every routed concern's suggested_patch git-applied
	// cleanly and the committed-tree verify gate passed; the agent was skipped.
	fixupApplyPathApplied = "applied"
	// fixupApplyPathAgent: no apply-list was served (a non-mechanical / mixed
	// fix-up) or no verify gate was configured, so the agent re-derived the change.
	fixupApplyPathAgent = "agent"
	// fixupApplyPathFailedFellback: an apply-list was served but the apply or its
	// verify gate failed; the worktree reset cleanly and the agent re-derived.
	fixupApplyPathFailedFellback = "apply_failed_fellback"
	// fixupApplyPathFailedResetFailed: an apply failed AND the post-failure
	// worktree reset also failed; the runner failed the stage loud rather than
	// run the agent on a possibly half-applied tree (so this value normally
	// never reaches a fixup_pushed report, but is recognized for completeness).
	fixupApplyPathFailedResetFailed = "apply_failed_reset_failed"
)

// normalizeFixupApplyPath returns the reported fix-up apply provenance value
// when it is one of the four recognized discriminators, else "" (which the
// caller treats as "omit the apply_path key"). It guards the fixup_pushed audit
// entry from persisting an absent or runner-bug value as if it were meaningful.
func normalizeFixupApplyPath(s string) string {
	switch s {
	case fixupApplyPathApplied, fixupApplyPathAgent,
		fixupApplyPathFailedFellback, fixupApplyPathFailedResetFailed:
		return s
	default:
		return ""
	}
}

// fixupScopeUsed returns the scope string that authorized the fix-up
// for the admissibility_reason receipt field.
func fixupScopeUsed(id Identity) string {
	if hasScope(id, "write:fixups") {
		return "write:fixups"
	}
	if hasScope(id, "write:stages") {
		return "write:stages"
	}
	return "session"
}
