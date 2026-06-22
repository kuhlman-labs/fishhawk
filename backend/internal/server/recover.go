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
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// CategoryPlanReusedFrom is the audit-log category for the provenance
// entry the recovery handler appends on the NEW run (E22.X / #978).
// Payload carries {parent_run_id, parent_failure_category, added_paths,
// exempted_paths, source, reason}. Internal audit kind — NOT an
// issue-comment surface.
const CategoryPlanReusedFrom = "plan_reused_from"

// recoverRunRequest is the JSON body of POST /v0/runs/{run_id}/recover.
// All fields are optional — an empty body recovers against the parent's
// approved plan with no scope amendment.
type recoverRunRequest struct {
	// AddScopeFiles are the operator-named paths folded into the
	// recovery run's effective scope via a pre-approved #961 scope
	// amendment row. Operation defaults to modify when omitted.
	AddScopeFiles []scopeamendment.PathEntry `json:"add_scope_files,omitempty"`
	// ExemptScopeFiles are the operator-justified-unchanged DECLARED
	// scope.files paths the runner's #1151 shortfall gate subtracts
	// (#1229) — the inverse of AddScopeFiles. Each {path, reason} is
	// validated (clean repo-relative path + non-empty reason) and
	// persisted on the plan_reused_from provenance as exempted_paths; it
	// does NOT mint a scope-amendment row (it subtracts from the gate, it
	// does not widen scope). scopeExemption is defined in prompt.go.
	ExemptScopeFiles []scopeExemption `json:"exempt_scope_files,omitempty"`
	// Reason rides on both the amendment row and the plan_reused_from
	// provenance entry, and is injected into the recovery agent's binding
	// conditions (Part D, #1229).
	Reason string `json:"reason,omitempty"`
	// BudgetOverride forces the recovery past a blocking periodic
	// budget that is over its limit, mirroring POST /v0/runs (#688).
	BudgetOverride bool `json:"budget_override,omitempty"`
}

// handleRecoverRun implements POST /v0/runs/{run_id}/recover (#978):
// operator-initiated category-B recovery. It mints a plan-stage-less
// child run that executes against the parent run's approved plan —
// the same inheritance shape as the CI-failure retry path
// (webhook.handleCIFailureRetry), reused rather than duplicated — with
// the operator's add_scope_files folded via a pre-approved #961 scope
// amendment row on the new implement stage.
//
// Eligibility is gated to parents whose plan stage succeeded and whose
// implement stage failed category-B: recovery is a NEW run against an
// already-approved plan, not a retry of the terminal one
// (fishhawk_retry_stage keeps refusing B).
//
// RetryAttempt is carried UNCHANGED from the parent — operator
// recovery is not an auto-retry and must not consume the
// on_ci_failure cap; ParentRunID threading is the provenance.
func (s *Server) handleRecoverRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:runs") {
		return
	}
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"recover endpoint requires a configured run repository", nil)
		return
	}
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_repo_unconfigured",
			"recover endpoint requires a configured audit repository", nil)
		return
	}

	parentID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var req recoverRunRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	// Validate + normalize the operator's paths up front — same
	// repo-relative containment contract as the mid-stage amendment
	// request (#961/#823). Operation defaults to modify.
	var amendPaths []scopeamendment.PathEntry
	if len(req.AddScopeFiles) > 0 {
		entries := make([]scopeamendment.PathEntry, len(req.AddScopeFiles))
		for i, e := range req.AddScopeFiles {
			if e.Operation == "" {
				e.Operation = scopeamendment.OperationModify
			}
			entries[i] = e
		}
		amendPaths, err = scopeamendment.ValidatePaths(entries)
		if err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(), map[string]any{"field": "add_scope_files"})
			return
		}
		// Fail before creating the run rather than minting a recovery
		// that silently drops the amendment.
		if s.cfg.ScopeAmendmentRepo == nil {
			s.writeError(w, r, http.StatusServiceUnavailable, "scope_amendment_unconfigured",
				"add_scope_files requires a configured scope-amendment repository", nil)
			return
		}
	}

	// Validate + normalize the operator's exempt_scope_files up front (#1229).
	// Each path must be clean repo-relative AND carry a non-empty reason — a
	// 400 on either failure, before the run is minted. Exemptions do NOT mint
	// a scope-amendment row; they ride the plan_reused_from provenance and the
	// runner subtracts them from the #1151 shortfall gate.
	exemptPaths, err := validateExemptScopeFiles(req.ExemptScopeFiles)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "exempt_scope_files"})
		return
	}

	parent, err := s.cfg.RunRepo.GetRun(r.Context(), parentID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": parentID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	// Decomposition-child target: recover IN PLACE rather than minting a
	// new run. A child that failed its own implement category-B is
	// re-opened via run.RedriveChild against the parent-walked approved
	// plan on the shared branch — branching here, before the new-child
	// eligibility gate, because a decomposition child has no plan stage
	// of its own and re-creates no stages from spec. An in-place re-drive
	// (not a freshly minted child sharing DecomposedFrom) is deliberate:
	// a second DecomposedFrom row would double-count in
	// childcompletion.resolveParent's consolidation counters.
	if parent.DecomposedFrom != nil {
		s.handleRecoverDecompositionChild(w, r, parent, amendPaths, exemptPaths, req.Reason)
		return
	}

	// Legacy rows without a cached spec can't recover — there is no
	// stage list to re-create. Start a fresh run instead.
	if len(parent.WorkflowSpec) == 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, "recovery_unsupported",
			"parent run has no cached workflow spec; start a fresh run instead",
			map[string]any{"run_id": parentID.String()})
		return
	}

	// Eligibility gate: plan stage succeeded AND implement stage
	// failed category-B.
	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), parentID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}
	var planStage, implementStage *run.Stage
	for _, st := range stages {
		switch st.Type {
		case run.StageTypePlan:
			if planStage == nil {
				planStage = st
			}
		case run.StageTypeImplement:
			if implementStage == nil {
				implementStage = st
			}
		}
	}
	planState := ""
	if planStage != nil {
		planState = string(planStage.State)
	}
	implementState := ""
	failureCategory := ""
	if implementStage != nil {
		implementState = string(implementStage.State)
		if implementStage.FailureCategory != nil {
			failureCategory = string(*implementStage.FailureCategory)
		}
	}
	eligible := planStage != nil && planStage.State == run.StageStateSucceeded &&
		implementStage != nil && implementStage.State == run.StageStateFailed &&
		implementStage.FailureCategory != nil && *implementStage.FailureCategory == run.FailureB
	if !eligible {
		s.writeError(w, r, http.StatusConflict, "recovery_not_eligible",
			"recovery requires a succeeded plan stage and an implement stage failed category-B",
			map[string]any{
				"plan_state":       planState,
				"implement_state":  implementState,
				"failure_category": failureCategory,
			})
		return
	}

	parsed, err := spec.ParseBytes(parent.WorkflowSpec)
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "recovery_unsupported",
			"parent run's cached workflow spec failed to parse",
			map[string]any{"error": err.Error()})
		return
	}
	workflowDef, ok := parsed.Workflows[parent.WorkflowID]
	if !ok {
		s.writeError(w, r, http.StatusUnprocessableEntity, "recovery_unsupported",
			"workflow_id not defined in parent run's cached workflow spec",
			map[string]any{"workflow_id": parent.WorkflowID})
		return
	}
	recoveryStages := webhook.FilterOutPlanStages(workflowDef.Stages)
	if len(recoveryStages) == 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, "recovery_unsupported",
			"workflow has no non-plan stages to recover against",
			map[string]any{"workflow_id": parent.WorkflowID})
		return
	}

	// Idempotency-Key (E8.2): replay returns the existing run with
	// 200 so a network-hiccup re-call can't mint two recovery runs.
	// Same (repo, key) keyspace as POST /v0/runs. Replay is honored
	// BEFORE the budget gate: a successful recovery followed by a
	// network retry must return the existing run even when the
	// blocking budget tripped between the two calls — replay is not
	// new spend.
	idempKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempKey != "" {
		existing, err := s.cfg.RunRepo.GetRunByIdempotencyKey(r.Context(), parent.Repo, idempKey)
		switch {
		case err == nil:
			s.writeJSON(w, r, http.StatusOK, toRunResponse(existing))
			return
		case errors.Is(err, run.ErrNotFound):
			// First call with this key — fall through to create.
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"idempotency lookup failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// Blocking periodic-budget admission gate (#688 / ADR-030):
	// recovery is new spend. checkBlockingBudget writes the error
	// response (and the audit) on refusal.
	if !s.checkBlockingBudget(w, r, parent.Repo, parent.WorkflowID, workflowDef.Budgets, req.BudgetOverride) {
		return
	}

	// Mint the recovery run, mirroring handleCIFailureRetry's
	// inheritance — except RetryAttempt, carried UNCHANGED so
	// operator recovery never consumes the on_ci_failure cap.
	pid := parent.ID
	createParams := run.CreateRunParams{
		Repo:                   parent.Repo,
		WorkflowID:             parent.WorkflowID,
		WorkflowSHA:            parent.WorkflowSHA,
		TriggerSource:          parent.TriggerSource,
		TriggerRef:             parent.TriggerRef,
		InstallationID:         parent.InstallationID,
		ParentRunID:            &pid,
		RequiredChecksSnapshot: parent.RequiredChecksSnapshot,
		WorkflowSpec:           parent.WorkflowSpec,
		RetryAttempt:           parent.RetryAttempt,
		MaxRetriesSnapshot:     parent.MaxRetriesSnapshot,
		RunnerKind:             parent.RunnerKind,
		IssueContext:           parent.IssueContext,
	}
	if idempKey != "" {
		k := idempKey
		createParams.IdempotencyKey = &k
	}
	child, err := s.cfg.RunRepo.CreateRun(r.Context(), createParams)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create recovery run failed", map[string]any{"error": err.Error()})
		return
	}

	// Create stages — skip plan. The recovery's implement stage prompt
	// walks ParentRunID to the parent's approved plan
	// (loadApprovedPlanForRun), exactly like a CI retry. No
	// workflow_dispatch is fired — identical to handleCreateRun:
	// local-runner runs sit pending for fishhawk_run_stage.
	createdStages, err := webhook.CreateStagesFromSpec(r.Context(), s.cfg.RunRepo, child.ID, recoveryStages)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create recovery stages failed", map[string]any{"error": err.Error()})
		return
	}

	id := IdentityFrom(r.Context())

	// Fold the operator's scope amendment as a pre-approved #961 row
	// on the new implement stage — exactly what
	// mergeApprovedScopeAmendments and the runner's pre-commit
	// refresh already consume; the operation=create entries flow into
	// the #818/#825 net-new-file gates like any approved amendment.
	if len(amendPaths) > 0 {
		var newImplement *run.Stage
		for _, st := range createdStages {
			if st.Type == run.StageTypeImplement {
				newImplement = st
				break
			}
		}
		if newImplement == nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"recovery run has no implement stage to attach the scope amendment to", nil)
			return
		}
		if err := s.createApprovedScopeAmendment(r.Context(), child.ID, newImplement.ID, amendPaths, req.Reason,
			"operator-named scope amendment at category-B recovery of run "+parent.ID.String(), id.Subject); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error", err.Error(), nil)
			return
		}
	}

	// Provenance: plan_reused_from on the NEW run. Best-effort — the
	// child's ParentRunID and the approved amendment row are the
	// durable records; a failed append warn-logs rather than
	// unwinding the created run.
	s.writePlanReusedFromAudit(r, child.ID, parent.ID.String(), "operator_recovery", amendPaths, exemptPaths, req.Reason, id.Subject)

	s.writeJSON(w, r, http.StatusCreated, toRunResponse(child))
}

// createApprovedScopeAmendment folds the operator-named paths into the
// effective scope of the given run's implement stage as a pre-approved
// #961 amendment row (create, then auto-approve). It is the shared core
// of both recover branches: the new-child mint attaches the row to the
// freshly created implement stage; the in-place decomposition re-drive
// attaches it to the child's EXISTING implement stage. amendPaths must
// be non-empty. defaultReason is used when the operator gave no reason.
func (s *Server) createApprovedScopeAmendment(ctx context.Context, runID, stageID uuid.UUID, amendPaths []scopeamendment.PathEntry, reason, defaultReason, decidedBy string) error {
	rsn := strings.TrimSpace(reason)
	if rsn == "" {
		rsn = defaultReason
	}
	amendment, err := s.cfg.ScopeAmendmentRepo.Create(ctx, scopeamendment.CreateParams{
		RunID:   runID,
		StageID: stageID,
		Paths:   amendPaths,
		Reason:  rsn,
	})
	if err != nil {
		return fmt.Errorf("create scope amendment failed: %w", err)
	}
	if _, err := s.cfg.ScopeAmendmentRepo.Decide(ctx, scopeamendment.DecideParams{
		ID:        amendment.ID,
		Status:    scopeamendment.StatusApproved,
		Reason:    "pre-approved by the recovering operator",
		DecidedBy: decidedBy,
	}); err != nil {
		return fmt.Errorf("approve scope amendment failed: %w", err)
	}
	return nil
}

// handleRecoverDecompositionChild is the recover-handler branch for a
// target that is itself a decomposition child (DecomposedFrom != nil).
// It recovers the child IN PLACE rather than minting a new run:
//
//  1. Eligibility — the child's own implement stage must be failed with
//     FailureCategory==B AND its approved plan must resolve via the
//     ParentRunID parent-walk (loadApprovedPlanForRun); otherwise
//     recovery_not_eligible names which leg failed.
//  2. Re-open the child via run.RedriveChild (failed implement →
//     pending, failed run → running) on the shared parent branch. This
//     runs FIRST and acts as the duplicate guard: it un-terminals the
//     run, so a concurrent/replayed recover fails here and never folds an
//     amendment.
//  3. Fold the operator's add_scope_files as a pre-approved amendment on
//     the EXISTING implement stage — only after a successful re-drive (so
//     a failed attempt never orphans an approved amendment) and BEFORE
//     the orchestrator handoff, so the implement prompt's
//     mergeApprovedScopeAmendments fold (keyed by run + stage id) sees it.
//  4. Append a plan_reused_from provenance entry
//     (source=decomposition_child_recovery).
//  5. Hand off to Orchestrator.Advance to walk pending → dispatched,
//     then return the re-opened child (same id).
//
// run.RedriveChild accepts any failure category — gating on category-B
// is this handler's job, not RedriveChild's.
//
// Authorization: this branch re-opens a terminal run via the same
// run.RedriveChild action POST /v0/runs/{id}/redrive performs, and that
// action is operator-only — an agent (MCP subject-bound) token must
// never re-drive any run (#698 / handleRedriveChild). The enclosing
// handler's write:runs gate is necessary but not sufficient: we reject
// agent-subject tokens here too so BOTH paths to RedriveChild enforce
// the identical contract. (Runner-side fhm_ tokens carry only mcp:read
// and so can't clear write:runs to reach this branch in practice — but
// the authz posture must be consistent by construction, not by accident.)
func (s *Server) handleRecoverDecompositionChild(w http.ResponseWriter, r *http.Request, child *run.Run, amendPaths []scopeamendment.PathEntry, exemptPaths []scopeExemption, reason string) {
	id := IdentityFrom(r.Context())
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		s.writeError(w, r, http.StatusForbidden, "agent_token_forbidden",
			"in-place re-drive of a decomposition child is an operator action; agent (mcp) tokens may not re-drive any run", nil)
		return
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), child.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}
	var implementStage *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			implementStage = st
			break
		}
	}
	implementState := ""
	failureCategory := ""
	if implementStage != nil {
		implementState = string(implementStage.State)
		if implementStage.FailureCategory != nil {
			failureCategory = string(*implementStage.FailureCategory)
		}
	}

	// Eligibility leg 1: the child's own implement stage failed
	// category-B. Leg 2: its approved plan resolves via the parent walk.
	categoryB := implementStage != nil && implementStage.State == run.StageStateFailed &&
		implementStage.FailureCategory != nil && *implementStage.FailureCategory == run.FailureB
	approvedPlan, err := s.loadApprovedPlanForRun(r.Context(), child.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve parent plan failed", map[string]any{"error": err.Error()})
		return
	}
	if !categoryB || approvedPlan == nil {
		s.writeError(w, r, http.StatusConflict, "recovery_not_eligible",
			"in-place recovery of a decomposition child requires the child's own implement stage failed category-B and an approved plan resolvable via the parent walk",
			map[string]any{
				"implement_state":  implementState,
				"failure_category": failureCategory,
				"plan_resolved":    approvedPlan != nil,
			})
		return
	}

	// Re-open the child in place against the shared parent branch FIRST.
	// RedriveChild transitions the run failed → running, so it is the
	// duplicate guard: a concurrent duplicate recover (or any replay after
	// the child has already re-opened) fails here with
	// ErrRedriveNotApplicable and never reaches the amendment fold below.
	// Folding the scope amendment ONLY after a successful re-drive means a
	// failed recover attempt can never leave an orphaned approved
	// amendment that would silently widen the re-driven prompt's scope.
	if _, err := run.RedriveChild(r.Context(), s.cfg.RunRepo, child.ID); err != nil {
		switch {
		case errors.Is(err, run.ErrRedriveNotApplicable):
			s.writeError(w, r, http.StatusConflict, "recovery_not_eligible", err.Error(), nil)
			return
		case errors.Is(err, run.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": child.ID.String()})
			return
		}
		var inv run.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_state_transition", err.Error(),
				map[string]any{"run_id": child.ID.String(), "from": inv.From, "to": inv.To})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"re-drive child failed", map[string]any{"error": err.Error()})
		return
	}

	// Fold the operator's amendment on the EXISTING implement stage —
	// after the successful re-drive (so no orphan on a failed attempt) and
	// BEFORE the Orchestrator handoff, so the implement prompt's
	// mergeApprovedScopeAmendments fold sees it on the re-opened stage (its
	// id is preserved by RetryStage's in-place reset, so implementStage.ID
	// is still valid post-redrive).
	if len(amendPaths) > 0 {
		if err := s.createApprovedScopeAmendment(r.Context(), child.ID, implementStage.ID, amendPaths, reason,
			"operator-named scope amendment at in-place recovery of decomposition child "+child.ID.String(), id.Subject); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error", err.Error(), nil)
			return
		}
	}

	// Provenance: plan_reused_from on the re-opened child. Best-effort —
	// the approved amendment row and the re-driven stage are the durable
	// records.
	s.writePlanReusedFromAudit(r, child.ID, child.DecomposedFrom.String(),
		"decomposition_child_recovery", amendPaths, exemptPaths, reason, id.Subject)

	// Un-terminal-ing the run let Advance act (it no-ops on terminal
	// runs); walk the re-opened pending implement stage → dispatched.
	// Best-effort, mirroring handleRedriveChild — the run is already
	// running and an operator can re-fire Advance.
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), child.ID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"orchestrator advance failed for decomposition-child recovery",
				slog.String("run_id", child.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	// Drive (#1271): the in-place re-drive re-opens the child's implement
	// stage to pending — the recover_redispatch transition point. The
	// Advance handoff above IS the auto-advance (workflow_dispatch for
	// runner_kind github_actions), so stamp the run_auto_advanced entry
	// that surfaces the required next action on the authoritative REST run
	// resource; runner_kind local parks with a host-side
	// run_implement_stage next action instead (ADR-024). Mirrors
	// recordDriveReviseReplan; the RedriveChild above already re-opened the
	// stage, so this fires unconditionally for a drive run.
	s.recordDriveDecompositionChildRecovery(r.Context(), child, implementStage.ID)

	// Return the re-opened child (re-fetched for its running state).
	updated, err := s.cfg.RunRepo.GetRun(r.Context(), child.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get re-opened child failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusCreated, toRunResponse(updated))
}

// recordDriveDecompositionChildRecovery stamps the drive engine's
// recover_redispatch rule (#1271) after an in-place decomposition-child
// re-drive re-opens the child's implement stage to pending. No-ops when no
// engine is wired or the child is not a drive run (best-effort: the
// re-drive already landed; a missing stamp degrades attribution, never the
// run). The child run is already in hand (fetched at the top of the recover
// handler), so its Drive + RunnerKind are read directly without a re-fetch.
// For runner_kind github_actions the entry records the advance the
// orchestrator's workflow_dispatch fired; for runner_kind local it records
// the park (Parked=true) with the run_implement_stage next action that
// surfaces on the REST run resource and MCP get_run_status. The entry is
// keyed to the re-opened implement stage.
func (s *Server) recordDriveDecompositionChildRecovery(ctx context.Context, child *run.Run, implementStageID uuid.UUID) {
	if s.drive == nil {
		return
	}
	if !child.Drive {
		return
	}
	out := drive.EvaluateRecoverRedispatch(child.RunnerKind)
	adv := drive.Advance{
		Rule: drive.RuleRecoverRedispatch,
		From: "implement:failed",
	}
	if out.Advance {
		adv.To = "implement:dispatched"
		adv.Event = "decomposition child re-driven in place; orchestrator re-dispatched the implement stage via workflow_dispatch"
	} else {
		adv.To = "implement:pending"
		adv.Event = "decomposition child re-driven in place; runner_kind local parks for a host-side re-dispatch"
		adv.Parked = true
		adv.NextAction = out.NextAction
	}
	s.drive.Record(ctx, child.ID, &implementStageID, adv)
}

// validateExemptScopeFiles normalizes and validates the operator's
// exempt_scope_files (#1229). Each entry must name a clean repo-relative path
// (non-empty after trim; not absolute; no ".." traversal — the same
// containment contract isRepoRelativePath enforces for the #1151 shortfall
// gate) AND carry a non-empty reason. Returns the trimmed entries or an error
// describing the first bad entry; nil input yields nil with no error (an
// exemption-less recovery is valid).
func validateExemptScopeFiles(in []scopeExemption) ([]scopeExemption, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]scopeExemption, 0, len(in))
	for _, e := range in {
		p := strings.TrimSpace(e.Path)
		if p == "" {
			return nil, errors.New("exempt_scope_files entries must name a non-empty repo-relative path")
		}
		if !isRepoRelativePath(p) {
			return nil, fmt.Errorf("exempt_scope_files path %q must be repo-relative (no leading '/' or '..')", p)
		}
		reason := strings.TrimSpace(e.Reason)
		if reason == "" {
			return nil, fmt.Errorf("exempt_scope_files path %q must carry a non-empty reason", p)
		}
		out = append(out, scopeExemption{Path: p, Reason: reason})
	}
	return out, nil
}

// writePlanReusedFromAudit appends the plan_reused_from chain entry on
// the recovery run (childID). source distinguishes the new-child mint
// (operator_recovery) from the in-place decomposition re-drive
// (decomposition_child_recovery); parentRunID is the run whose approved
// plan was reused. exemptedPaths records the operator's exempt_scope_files
// (#1229) — the durable record the prompt builder reads back to deliver
// scope_exemptions to the runner gate.
func (s *Server) writePlanReusedFromAudit(r *http.Request, childID uuid.UUID, parentRunID, source string, addedPaths []scopeamendment.PathEntry, exemptedPaths []scopeExemption, reason, subject string) {
	actorKind := audit.ActorUser
	payload, _ := json.Marshal(map[string]any{
		"parent_run_id":           parentRunID,
		"parent_failure_category": string(run.FailureB),
		"added_paths":             addedPaths,
		"exempted_paths":          exemptedPaths,
		"source":                  source,
		"reason":                  reason,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        childID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryPlanReusedFrom,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan_reused_from audit append failed",
			slog.String("run_id", childID.String()),
			slog.String("parent_run_id", parentRunID),
			slog.String("error", err.Error()))
	}
}
