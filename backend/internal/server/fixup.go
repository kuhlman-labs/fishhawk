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
// Concerns selects which recorded implement-review concerns (by their
// index in the stage's resolved concern set) to route back to the
// agent; it must be non-empty. Reason is an optional operator note
// recorded on the audit entry. AllowCreate declares net-new files this
// fix-up pass will create (#823); the paths are folded into the
// effective scope.files for THAT dispatch only so the runner's #818
// created-out-of-scope gate stages them rather than failing category-B.
// Any created file NOT declared here still trips the gate.
type fixupRequest struct {
	Concerns    []int    `json:"concerns"`
	Reason      string   `json:"reason"`
	AllowCreate []string `json:"allow_create"`
	// ForceAdditionalPass is the bounded operator override (#860): when
	// true it grants ONE fix-up pass beyond the normal budget
	// (defaultMaxFixupPasses), hard-capped at defaultFixupCeiling total
	// passes per stage. The forced pass is audited (a `forced` flag plus
	// the operator reason). Default false preserves the prior behaviour.
	ForceAdditionalPass bool `json:"force_additional_pass"`
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
	if len(reqBody.Concerns) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"concerns must select at least one recorded implement-review concern",
			map[string]any{"field": "concerns"})
		return
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

	// Resolve the stage's recorded implement-review concerns from the
	// implement_reviewed audit entries (approve_with_concerns verdicts),
	// in append order. This is the set the operator's indices address.
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
	selected, err := selectConcerns(concerns, reqBody.Concerns)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(),
			map[string]any{"field": "concerns", "available": len(concerns)})
		return
	}

	// Count prior fix-up passes for this stage to enforce the bound.
	priorPasses, err := s.countFixupPasses(r.Context(), stage.RunID, stageID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count prior fix-up passes failed", map[string]any{"error": err.Error()})
		return
	}

	dec, err := run.FixupStage(r.Context(), s.cfg.RunRepo, stageID, run.FixupOptions{
		PriorPassCount:      priorPasses,
		MaxPasses:           defaultMaxFixupPasses,
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
				err.Error(), map[string]any{"max_passes": defaultMaxFixupPasses, "used": priorPasses})
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
	s.writeFixupAudit(r, dec, selected, reqBody.Concerns, reqBody.Reason, allowCreate, priorPasses)

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

	// Sticky status comment (E20.4 / #330): a fix-up flips the stage back
	// to pending / dispatched; the status comment should reflect that.
	s.notifyStatusUpdate(r.Context(), dec.Stage.RunID, "stage_fixup")

	s.writeJSON(w, r, http.StatusOK, toStageResponse(dec.Stage))
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
// counter the bound is enforced against.
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
// selected concern indices, the resolved concern objects (so the prompt
// renderer delivers them as binding instructions), the operator reason,
// the declared allow-create paths (#823, folded into the effective
// scope.files for the fix-up dispatch), and the bounded-pass receipt
// fields. Best-effort: the transition is already committed, so a failure
// here logs but doesn't unwind.
func (s *Server) writeFixupAudit(r *http.Request, dec *run.FixupDecision, selected []planreview.Concern, indices []int, reason string, allowCreate []string, priorPasses int) {
	id := IdentityFrom(r.Context())
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser

	passOrdinal := priorPasses + 1
	admissibilityReason := fmt.Sprintf("fix-up pass %d of %d; %d concern(s) routed back; via %s",
		passOrdinal, defaultMaxFixupPasses, len(selected), fixupScopeUsed(id))
	if dec.Forced {
		// Durably record that this pass ran past the normal budget only
		// because the operator forced it (#860).
		admissibilityReason += "; operator-forced override"
	}
	fields := map[string]any{
		"stage_id":             dec.Stage.ID.String(),
		"prior_state":          string(dec.PriorState),
		"selected_indices":     indices,
		"concerns":             selected,
		"reason":               reason,
		"allow_create":         allowCreate,
		"pass_ordinal":         passOrdinal,
		"max_passes":           defaultMaxFixupPasses,
		"hard_ceiling":         defaultFixupCeiling,
		"remaining_budget":     dec.RemainingBudget,
		"forced":               dec.Forced,
		"admissibility_reason": admissibilityReason,
	}
	// push_and_open_pr flow (#780): record the review stage re-parked
	// alongside the implement re-open, so the audit trail captures the
	// full state change (and downstream tooling can correlate the gate).
	if dec.ReparkedReview != nil {
		fields["reparked_review_stage_id"] = dec.ReparkedReview.ID.String()
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
