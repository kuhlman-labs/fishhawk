package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// maxScopeCompletenessAmendsPerStage bounds how many times ONE parked implement
// stage may be amended-and-resumed (#2591). A re-run under a widened scope can
// park again, and an unbounded loop would burn a full agent pass per attempt, so
// the third amend is refused 409 amend_budget_exhausted and the operator falls
// back to `fail` plus a corrected decomposition.
//
// It is counted over DISTINCT amendment_id values carried by this stage's
// scope_completeness_amended audit entries, NOT over raw entries: a mid-sequence
// failure that is retried re-uses the same pre-approved row (see
// resolveOrCreateOperatorAmendRow), so a retry appends a second entry for the
// SAME amendment and must not consume a second slot. An entry whose payload
// carries no decodable amendment_id counts on its own — the count fails closed.
const maxScopeCompletenessAmendsPerStage = 2

// operatorAmendReasonPrefix marks the pre-approved #961 row an operator amend
// mints, so a RETRY after a failure later in the sequence can recognize its own
// orphaned row and reuse it instead of minting a second (binding condition 2).
// A second row would be inert for the effective scope (foldScopeEntries dedupes
// by path) but NOT for the budget, which counts ROWS via
// ScopeAmendmentRepo.CountByStage — so every failed-then-retried cycle would
// silently eat one of the re-run agent's two #961 slots.
const operatorAmendReasonPrefix = "scope-completeness amend: "

// amendUnresolvedOwner is one submitted path whose OWNING slice the guard could
// not establish, with the reason it could not. Surfaced on the 409 details, and
// — when the operator acknowledges the risk — on the response and the
// scope_completeness_amended audit entry.
type amendUnresolvedOwner struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// The reasons an owner-slice attribution can fail to resolve. Each names a
// DISTINCT uncertainty so the operator acknowledging the risk knows what they
// are acknowledging.
const (
	amendOwnerUnresolvedNoEntry        = "no_owning_slice_entry"
	amendOwnerUnresolvedNoSliceIndex   = "owning_slice_index_absent"
	amendOwnerUnresolvedSiblingLookup  = "sibling_lookup_failed"
	amendOwnerUnresolvedSiblingMissing = "owning_slice_run_not_found"
	amendOwnerUnresolvedStagesUnread   = "owning_slice_stages_unreadable"
	amendOwnerUnresolvedNoImplement    = "owning_slice_implement_stage_absent"
)

// amendOwnerGuardResult is the owner-slice guard's verdict for the whole
// submitted path set: the RESOLVED attributions (echoed so the operator knows
// which sibling will see the same file on another branch) and the UNRESOLVED
// ones (which refuse the amend unless explicitly acknowledged).
type amendOwnerGuardResult struct {
	resolved   []run.BuildRequiredOwner
	unresolved []amendUnresolvedOwner
}

// amendOwnerActiveRefusal is the guard's hard refusal: a submitted path whose
// owning sibling slice has already STARTED its implement pass, i.e. the window
// in which two branches can carry divergent edits to one file.
type amendOwnerActiveRefusal struct {
	Path        string `json:"path"`
	SliceIndex  int    `json:"slice_index"`
	SliceTitle  string `json:"slice_title,omitempty"`
	SiblingRun  string `json:"sibling_run_id"`
	SiblingStat string `json:"sibling_implement_state"`
}

// amendScopeCompleteness resolves an `amend` decision (#2591): it WIDENS the
// parked implement stage's effective scope with a pre-approved #961
// scope-amendment row and resumes the stage, so the agent re-runs against the
// wider scope. This is what gives the BUILD-REQUIRED shortfall class (#2548) a
// resolution other than `fail` — `exempt` is refused for that class outright
// because its held tree is red by construction.
//
// WHOSE SCOPE WIDENS: the parked CHILD's own implement stage, never the
// parent's approved decomposition (an approved plan artifact is immutable). The
// widening is stage-scoped and lasts for the remainder of this child's implement
// pass.
//
// THE HELD COMMIT IS REUSED, NOT DISCARDED: nothing here resets, deletes, or
// rewrites stage.ScopeCompletenessPark, so the slice's work stays on its
// sole-writer run branch (ADR-035). The park row also stays as the durable
// record of what was parked; prompt.go's invalidator list is what stops it being
// read as a live exemption after an amend.
//
// ORDERING IS LOAD-BEARING, in this exact sequence:
//
//	(a) validate paths        — 400, nothing touched
//	(b) coverage check        — 409, nothing touched
//	(c) per-stage amend cap    — 409, nothing touched
//	(d) owner-slice guard      — 409, nothing touched
//	(e) pre-approved row       — 500 amend_unrecorded, stage left PARKED
//	(f) amended audit entry    — 500 amend_unrecorded, stage left PARKED
//	(g) resume to pending      — 500 amend_resume_failed, stage left PARKED
//	(h) orchestrator advance   — best-effort WARN (mirrors both existing arms)
//
// Steps (a)-(d) are the no-side-effect refusals; a stage whose widening the
// chain never recorded is never dispatched. Every step from (e) on is RETRY-SAFE:
// re-POSTing the same amend reuses the row (e), re-appends an entry keyed to the
// same amendment_id so the cap is unmoved (c/f), and re-attempts the resume (g).
func (s *Server) amendScopeCompleteness(w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	runRow *run.Run, stage *run.Stage, reqBody scopeCompletenessDecisionRequest, decidedBy string) {
	if s.cfg.ScopeAmendmentRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "scope_completeness_unconfigured",
			"the amend decision requires the scope-amendment repository", nil)
		return
	}
	ctx := r.Context()

	// (a) The SAME validator #961 uses, so the two channels share vocabulary and
	// containment rules rather than drifting into two path grammars.
	paths, err := scopeamendment.ValidatePaths(reqBody.Paths)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"paths must name at least one valid repo-relative {path, operation} entry to fold into the parked stage's scope",
			map[string]any{"field": "paths", "error": err.Error()})
		return
	}

	park := stage.ScopeCompletenessPark

	// (b) COVERAGE. A re-run under a scope that still excludes a build-required
	// file re-parks identically and burns a full agent pass, so an amend that
	// does not cover every build_required_path is refused before anything is
	// written.
	if park != nil && len(park.BuildRequiredPaths) > 0 {
		if uncovered := uncoveredBuildRequiredPaths(park.BuildRequiredPaths, paths); len(uncovered) > 0 {
			s.writeError(w, r, http.StatusConflict, "amend_incomplete_coverage",
				"the amended paths do not cover every build-required path this park recorded: a re-run under a scope that still excludes one of them re-parks identically and burns a full agent pass",
				map[string]any{
					"shortfall_class":      ShortfallClassBuildRequired,
					"uncovered_paths":      uncovered,
					"build_required_paths": park.BuildRequiredPaths,
				})
			return
		}
	}

	// (c) PER-STAGE AMEND CAP.
	used, err := s.countScopeCompletenessAmends(ctx, runID, stage.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count prior scope-completeness amendments failed",
			map[string]any{internalCauseKey: err.Error()})
		return
	}
	if used >= maxScopeCompletenessAmendsPerStage {
		s.writeError(w, r, http.StatusConflict, "amend_budget_exhausted",
			"this stage has exhausted its amend-and-resume budget; the remaining resolution is \"fail\" plus a corrected decomposition",
			map[string]any{"max": maxScopeCompletenessAmendsPerStage, "used": used})
		return
	}

	// (d) OWNER-SLICE GUARD. The run row comes from resolveParkedScopeStage,
	// which already refused a GetRun failure — re-fetching it here would add a
	// round-trip whose error branch no request could reach.
	guard, refusal := s.resolveAmendOwnerGuard(ctx, runRow, park, paths)
	if refusal != nil {
		s.writeError(w, r, http.StatusConflict, "amend_refused_owner_slice_active",
			"a submitted path is owned by a sibling slice whose implement pass has already STARTED: from that moment two branches can carry divergent edits to one file and consolidation would conflict. Drop the file from that slice, or merge the slices, and re-run",
			map[string]any{
				"path":                    refusal.Path,
				"slice_index":             refusal.SliceIndex,
				"slice_title":             refusal.SliceTitle,
				"sibling_run_id":          refusal.SiblingRun,
				"sibling_implement_state": refusal.SiblingStat,
			})
		return
	}
	// FAIL CLOSED on unresolved ownership (binding condition 1). Unknown
	// ownership is exactly the case in which a sibling might already hold
	// divergent edits, so recording the risk is not preventing it: the amend is
	// REFUSED unless the operator re-admits it explicitly, and the
	// acknowledgement is then audited rather than defaulted.
	if len(guard.unresolved) > 0 && !reqBody.AcknowledgeOwnerUnresolved {
		s.writeError(w, r, http.StatusConflict, "amend_owner_attribution_unresolved",
			"the owning slice of at least one submitted path could not be resolved, so this amend cannot be shown to preserve the single-owner-file invariant. Re-submit with acknowledge_owner_unresolved:true to take that risk knowingly (it is recorded on the audit entry), or resolve the attribution first",
			map[string]any{
				"unresolved_paths":       guard.unresolved,
				"acknowledgement_field":  "acknowledge_owner_unresolved",
				"resolved_owning_slices": guard.resolved,
			})
		return
	}
	acknowledged := len(guard.unresolved) > 0 && reqBody.AcknowledgeOwnerUnresolved

	// (e) The pre-approved #961 row — the SAME create+auto-approve mechanism
	// recover.go uses, so no prompt.go fold change is needed: the row is already
	// APPROVED, and resolveApprovedScopeAmendmentEntries filters on status alone
	// with no origin discriminator.
	amendmentID, err := s.resolveOrCreateOperatorAmendRow(ctx, runID, stage.ID, paths,
		reqBody.Reason, decidedBy)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "amend_unrecorded",
			"the scope widening could not be recorded; the stage is left parked — re-POST the decision",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	// (f) The amended audit entry. REFUSED on failure for the same reason the
	// exempt arm refuses: a stage whose widening the chain never recorded must
	// never become dispatchable, because that entry is also what DEMOTES a stale
	// `exempted` decision in prompt.go's newest-wins walk.
	extra := map[string]any{
		"paths":        paths,
		"amendment_id": amendmentID.String(),
	}
	if len(guard.resolved) > 0 {
		extra["owning_slices"] = guard.resolved
	}
	if acknowledged {
		extra["owner_attribution_unresolved"] = true
		extra["owner_unresolved_paths"] = guard.unresolved
		extra["acknowledged_owner_unresolved"] = true
	}
	if err := s.writeScopeCompletenessDecisionAudit(r, runID, stage,
		CategoryScopeCompletenessAmended, reqBody.Reason, decidedBy, extra); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "amend_unrecorded",
			"the scope widening could not be recorded in the audit chain; the stage is left parked — re-POST the decision (the amendment row is reused, not duplicated)",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	// (g) Pending, NOT Running — the #2501 reason: host_dispatch.go's admission
	// switch accepts only {pending, awaiting_host_dispatch}.
	if _, err := s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStatePending, nil); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "amend_resume_failed",
			"the widening was recorded but the parked stage could not be resumed; the stage is left parked — re-POST the decision (the amendment row is reused, not duplicated)",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	// (h) Best-effort advance, mirroring both existing arms.
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(ctx, runID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"scope-completeness amend decision: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	s.notifyStatusUpdate(ctx, runID, "scope_amended")

	resp := scopeCompletenessDecisionResponse{
		StageID:                       stage.ID,
		Decision:                      "amend",
		State:                         string(run.StageStatePending),
		AmendedPaths:                  paths,
		AmendmentID:                   amendmentID.String(),
		OwningSlices:                  guard.resolved,
		OwnerAttributionUnresolved:    acknowledged,
		AcknowledgedOwnerUnresolved:   acknowledged,
		AgentAmendmentBudgetRemaining: s.agentAmendmentBudgetRemaining(ctx, stage.ID),
	}
	if acknowledged {
		resp.OwnerUnresolvedPaths = guard.unresolved
	}
	if park != nil {
		resp.MissingPaths = park.MissingPaths
		resp.UnsatisfiedAssertions = park.UnsatisfiedAssertions
		resp.BuildRequiredPaths = park.BuildRequiredPaths
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// uncoveredBuildRequiredPaths returns the park's build-required paths the
// submitted amendment does not name, in the park's own order.
func uncoveredBuildRequiredPaths(required []string, submitted []scopeamendment.PathEntry) []string {
	have := make(map[string]struct{}, len(submitted))
	for _, p := range submitted {
		have[p.Path] = struct{}{}
	}
	var out []string
	for _, p := range required {
		if _, ok := have[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

// countScopeCompletenessAmends counts the DISTINCT amendments already applied to
// this stage — see maxScopeCompletenessAmendsPerStage for why the key is
// amendment_id and not the raw entry count.
func (s *Server) countScopeCompletenessAmends(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryScopeCompletenessAmended)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]struct{}, len(entries))
	n := 0
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var payload struct {
			AmendmentID string `json:"amendment_id"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.AmendmentID == "" {
			// Unkeyable: count it on its own rather than folding it into another
			// amendment's slot — the cap fails CLOSED.
			n++
			continue
		}
		if _, dup := seen[payload.AmendmentID]; dup {
			continue
		}
		seen[payload.AmendmentID] = struct{}{}
		n++
	}
	return n, nil
}

// agentAmendmentBudgetRemaining reports how many of the stage's TWO #961
// mid-stage amendment slots the RE-RUN agent still has. An operator amend
// consumes one, because maxScopeAmendmentsPerStage is enforced against ROW count
// via CountByStage and the amend row hangs off the same stage. Best-effort: a
// count failure reports 0 rather than failing the amend, since the number is
// observability, not a control.
func (s *Server) agentAmendmentBudgetRemaining(ctx context.Context, stageID uuid.UUID) int {
	used, err := s.cfg.ScopeAmendmentRepo.CountByStage(ctx, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"scope-completeness amend: scope-amendment row count failed; reporting no remaining agent budget",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
		return 0
	}
	if remaining := maxScopeAmendmentsPerStage - used; remaining > 0 {
		return remaining
	}
	return 0
}

// resolveAmendOwnerGuard evaluates the SINGLE-OWNER-FILE invariant for the
// submitted paths.
//
// The invariant this narrowly relaxes is that exactly one decomposition slice
// edits a given file. An amend hands a second slice permission to edit a file a
// sibling owns, which is safe ONLY while that sibling has not yet begun its
// implement pass: from Stage.StartedAt onwards two branches can carry divergent
// edits to one file and consolidation would conflict. StartedAt is the sound
// proxy because stages.started_at is written under COALESCE(started_at, $3) and
// never overwritten (see scope_amendment.go::amendmentDeadlineRemaining), so
// non-nil means an agent pass began. Its documented caveat — a re-spawned
// stage's started_at is CUMULATIVE — can only make this guard MORE conservative.
//
// FAIL-CLOSED (binding condition 1). Every branch that cannot establish
// ownership lands in `unresolved`, and the caller refuses the amend unless the
// operator explicitly acknowledges the risk. The ONE case treated as resolved
// by construction is a run that is not a decomposition child at all
// (DecomposedFrom == nil): no sibling slice exists, so no sibling can hold a
// divergent edit — there is nothing uncertain to fail closed on. A path owned by
// THIS run's own slice is likewise resolved: a slice cannot conflict with itself.
func (s *Server) resolveAmendOwnerGuard(ctx context.Context, runRow *run.Run,
	park *run.ScopeCompletenessPark, paths []scopeamendment.PathEntry) (amendOwnerGuardResult, *amendOwnerActiveRefusal) {
	var out amendOwnerGuardResult
	if runRow == nil || runRow.DecomposedFrom == nil {
		return out, nil
	}

	byPath := map[string]run.BuildRequiredOwner{}
	if park != nil {
		for _, o := range park.OwningSlices {
			byPath[o.Path] = o
		}
	}

	siblings, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{DecomposedFrom: runRow.DecomposedFrom})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"scope-completeness amend: sibling slice lookup failed; every path's ownership is unresolved",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		for _, p := range paths {
			out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedSiblingLookup})
		}
		return out, nil
	}

	for _, p := range paths {
		owner, ok := byPath[p.Path]
		if !ok {
			out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedNoEntry})
			continue
		}
		if owner.SliceIndex == nil {
			out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedNoSliceIndex})
			continue
		}
		// Self-owned: this child's own slice declares the path, so no sibling can
		// hold a divergent edit to it.
		if runRow.SliceIndex != nil && *runRow.SliceIndex == *owner.SliceIndex {
			out.resolved = append(out.resolved, owner)
			continue
		}
		sibling := siblingForSlice(siblings, runRow.ID, *owner.SliceIndex)
		if sibling == nil {
			out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedSiblingMissing})
			continue
		}
		stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, sibling.ID)
		if err != nil {
			out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedStagesUnread})
			continue
		}
		implement := implementStageOf(stages)
		if implement == nil {
			out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedNoImplement})
			continue
		}
		if implement.StartedAt != nil || implement.State == run.StageStateSucceeded {
			return out, &amendOwnerActiveRefusal{
				Path:        p.Path,
				SliceIndex:  *owner.SliceIndex,
				SliceTitle:  owner.SliceTitle,
				SiblingRun:  sibling.ID.String(),
				SiblingStat: string(implement.State),
			}
		}
		out.resolved = append(out.resolved, owner)
	}
	return out, nil
}

// siblingForSlice finds the fan-out child occupying the given sub-plan index,
// skipping the amending run itself.
func siblingForSlice(siblings []*run.Run, selfID uuid.UUID, sliceIndex int) *run.Run {
	for _, sib := range siblings {
		if sib == nil || sib.ID == selfID || sib.SliceIndex == nil {
			continue
		}
		if *sib.SliceIndex == sliceIndex {
			return sib
		}
	}
	return nil
}

func implementStageOf(stages []*run.Stage) *run.Stage {
	for _, st := range stages {
		if st != nil && st.Type == run.StageTypeImplement {
			return st
		}
	}
	return nil
}

// resolveOrCreateOperatorAmendRow returns the pre-approved #961 row carrying the
// widening, creating it only when this stage does not already have an identical
// one.
//
// IDEMPOTENT BY REUSE (binding condition 2). The row, the audit entry and the
// stage transition are three separate writes with no shared transaction, so a
// failure at either of the later two leaves an orphaned APPROVED row behind. A
// duplicate row is inert for the effective scope (foldScopeEntries dedupes by
// path) but NOT for the budget, which counts ROWS — so a naive retry would eat
// one of the re-run agent's two #961 slots per failed cycle. Reuse is keyed on
// (stage, marker reason, identical path set), which is exactly what a re-POST of
// the same decision presents. A PENDING orphan (Create succeeded, Decide failed)
// is completed rather than duplicated.
//
// A retry that changes the paths or the reason is a DIFFERENT amendment and
// correctly mints its own row; the orphan then stays and does consume a slot.
// That residual is surfaced, not hidden: the response's
// agent_amendment_budget_remaining reports the true remaining budget.
func (s *Server) resolveOrCreateOperatorAmendRow(ctx context.Context, runID, stageID uuid.UUID,
	paths []scopeamendment.PathEntry, reason, decidedBy string) (uuid.UUID, error) {
	markerReason := operatorAmendReasonPrefix + strings.TrimSpace(reason)

	existing, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runID)
	if err != nil {
		// Refuse rather than risk a duplicate: without the prior rows we cannot
		// tell a fresh amend from a retry, and guessing wrong drains the agent's
		// budget silently.
		return uuid.Nil, err
	}
	for _, a := range existing {
		if a == nil || a.StageID != stageID || a.Reason != markerReason || !samePathSet(a.Paths, paths) {
			continue
		}
		switch a.Status {
		case scopeamendment.StatusApproved:
			return a.ID, nil
		case scopeamendment.StatusPending:
			if _, err := s.cfg.ScopeAmendmentRepo.Decide(ctx, scopeamendment.DecideParams{
				ID:        a.ID,
				Status:    scopeamendment.StatusApproved,
				Reason:    "pre-approved by the amending operator",
				DecidedBy: decidedBy,
			}); err != nil {
				return uuid.Nil, err
			}
			return a.ID, nil
		}
	}

	amendment, err := s.cfg.ScopeAmendmentRepo.Create(ctx, scopeamendment.CreateParams{
		RunID:   runID,
		StageID: stageID,
		Paths:   paths,
		Reason:  markerReason,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := s.cfg.ScopeAmendmentRepo.Decide(ctx, scopeamendment.DecideParams{
		ID:        amendment.ID,
		Status:    scopeamendment.StatusApproved,
		Reason:    "pre-approved by the amending operator",
		DecidedBy: decidedBy,
	}); err != nil {
		return uuid.Nil, err
	}
	return amendment.ID, nil
}

// samePathSet compares two path sets as SETS of {path, operation}, so ordering
// and duplicates never make a retry look like a new amendment.
func samePathSet(a, b []scopeamendment.PathEntry) bool {
	key := func(in []scopeamendment.PathEntry) []string {
		out := make([]string, 0, len(in))
		for _, e := range in {
			out = append(out, e.Path+"\x00"+e.Operation)
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	if len(ka) != len(kb) {
		return false
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}
