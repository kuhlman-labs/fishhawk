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
	"github.com/kuhlman-labs/fishhawk/backend/internal/runnerbackend"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/wavecoverage"
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
//
// The marker alone is NOT a sufficient reuse key: a stage that is amended,
// re-runs and PARKS AGAIN presents the identical (stage, reason, path set) on
// its second episode, and reusing the first episode's row there would keep the
// cap counting ONE distinct amendment forever — an unbounded amend loop, which
// is precisely what maxScopeCompletenessAmendsPerStage exists to stop. So the
// marker also carries a PARK-EPISODE key (see amendEpisodeKey): a retry within
// one episode matches, a fresh park never does.
const operatorAmendReasonPrefix = "scope-completeness amend: "

// amendEpisodeKey identifies the CURRENT park episode, so an incomplete RETRY of
// this episode's amend is distinguishable from a NEW park that happens to submit
// byte-identical paths and reason.
//
// The held commit is the episode's identity: each park records the commit the
// agent's pass actually pushed, and a re-run under the widened scope produces a
// different one (that re-run is the whole point of an amend — it edits files the
// prior pass could not). VerifiedTreeSHA is the fallback for a park row that
// carries no held commit.
//
// An empty key means the episode CANNOT be identified. Reuse is then refused
// outright and every attempt mints its own row: that direction over-counts
// against the cap (fail-CLOSED on the control) and only over-spends the agent's
// #961 budget on a retry (observability, and reported honestly by
// agent_amendment_budget_remaining) — whereas reusing on an unidentifiable
// episode would silently uncap the amend loop.
func amendEpisodeKey(park *run.ScopeCompletenessPark) string {
	if park == nil {
		return ""
	}
	if park.HeldCommitSHA != "" {
		return park.HeldCommitSHA
	}
	return park.VerifiedTreeSHA
}

// operatorAmendMarkerReason renders the marker reason stored on the row: the
// prefix, this park episode's key, and the operator's own prose.
func operatorAmendMarkerReason(episode, reason string) string {
	return operatorAmendReasonPrefix + "[" + episode + "] " + strings.TrimSpace(reason)
}

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

// The reasons a plan-declared ancestor's delivery into this child's base could
// not be proven (Captain's ruling on #4798). Each is a HARD refusal:
// acknowledge_owner_unresolved cannot admit any of them.
const (
	amendAncestorNoWaveGate    = "ancestor_no_wave_gate"
	amendAncestorRunNotFound   = "ancestor_run_not_found"
	amendAncestorNoImplement   = "ancestor_implement_stage_absent"
	amendAncestorNotDelivered  = "ancestor_not_delivered"
	amendAncestorNotIntegrated = "ancestor_not_integrated"

	amendAncestorChannelUnreadable     = "child_dispatch_channel_unreadable"
	amendAncestorSiblingsUnreadable    = "ancestor_sibling_lookup_failed"
	amendAncestorStagesUnreadable      = "ancestor_stages_unreadable"
	amendAncestorIntegrationUnreadable = "ancestor_integration_unreadable"
)

// amendOwnerGuardResult is the owner-slice guard's verdict for the whole
// submitted path set: the RESOLVED attributions (echoed so the operator knows
// which sibling will see the same file on another branch) and the UNRESOLVED
// ones (which refuse the amend unless explicitly acknowledged).
type amendOwnerGuardResult struct {
	resolved   []run.BuildRequiredOwner
	unresolved []amendUnresolvedOwner
}

// amendOwnerActiveRefusal is the guard's hard refusal, of two kinds. Legacy: a
// submitted path the plan does not declare, whose park-attributed non-ancestor
// sibling has already STARTED its implement pass, i.e. the window in which two
// branches can carry divergent edits to one file. Ancestor: a plan-declared
// ancestor whose delivery into this child's base could not be proven.
//
// Neither ever enters amendOwnerGuardResult.unresolved, the only input to
// acknowledge_owner_unresolved, so the flag cannot admit either.
type amendOwnerActiveRefusal struct {
	Path           string `json:"path"`
	SliceIndex     int    `json:"slice_index"`
	SliceTitle     string `json:"slice_title,omitempty"`
	SiblingRun     string `json:"sibling_run_id"`
	SiblingStat    string `json:"sibling_implement_state"`
	SiblingRunStat string `json:"sibling_run_state"`
	// Ancestor marks a refused ancestor proof, with its Reason and whether a
	// later re-POST of the same decision can succeed (Retryable).
	Ancestor  bool   `json:"-"`
	Reason    string `json:"-"`
	Retryable bool   `json:"-"`
	// ReadFailed marks a proof read that errored, answered 500 rather than 409.
	// cause reaches only the log record, through internalCauseKey.
	ReadFailed bool `json:"-"`
	cause      string
	// branchPresent is set only for ancestor_not_integrated.
	branchPresent *bool
}

// writeAmendOwnerRefusal maps a guard refusal to its response. See the contract
// in docs/review.md: no refusal writes an amendment row, moves the stage, spends
// a cap slot or carries a raw cause, and none of them writes an audit row, because
// a failed read is not a decision and the ancestor 409s stay in the #2591 family.
func (s *Server) writeAmendOwnerRefusal(w http.ResponseWriter, r *http.Request, refusal *amendOwnerActiveRefusal) {
	if refusal.ReadFailed {
		s.writeError(w, r, http.StatusInternalServerError, errCodeSliceOwnershipCheckFailed,
			"the delivery of an ancestor slice could not be read, so this amend cannot be shown to edit a file already in this child's base; nothing was written. "+
				"acknowledge_owner_unresolved does not relax this",
			map[string]any{
				"retryable":      refusal.Retryable,
				"reason":         refusal.Reason,
				"remedy":         "re-POST the same decision once the read recovers; nothing was written",
				"path":           refusal.Path,
				"slice_index":    refusal.SliceIndex,
				internalCauseKey: refusal.cause,
			})
		return
	}
	if !refusal.Ancestor {
		s.writeError(w, r, http.StatusConflict, "amend_refused_owner_slice_active",
			"a submitted path is owned by a sibling slice whose implement pass has already STARTED: from that moment two branches can carry divergent edits to one file and consolidation would conflict. Drop the file from that slice, or merge the slices, and re-run",
			map[string]any{
				"path":                    refusal.Path,
				"slice_index":             refusal.SliceIndex,
				"slice_title":             refusal.SliceTitle,
				"sibling_run_id":          refusal.SiblingRun,
				"sibling_implement_state": refusal.SiblingStat,
				"sibling_run_state":       refusal.SiblingRunStat,
			})
		return
	}
	details := map[string]any{
		"path":                    refusal.Path,
		"slice_index":             refusal.SliceIndex,
		"slice_title":             refusal.SliceTitle,
		"sibling_run_id":          refusal.SiblingRun,
		"sibling_implement_state": refusal.SiblingStat,
		"sibling_run_state":       refusal.SiblingRunStat,
		"reason":                  refusal.Reason,
		"retryable":               refusal.Retryable,
		"remedy":                  amendAncestorRemedy(refusal.Retryable),
	}
	if refusal.branchPresent != nil {
		details["consolidated_branch_present"] = *refusal.branchPresent
	}
	s.writeError(w, r, http.StatusConflict, "amend_refused_owner_slice_active", amendAncestorMessage(refusal.Reason), details)
}

// amendAncestorRemedy is the remedy of an ancestor 409. A retryable reason clears
// without changing the plan or the runner kind.
func amendAncestorRemedy(retryable bool) string {
	if retryable {
		return "retry this decision once the ancestor slice has succeeded and the parent's newest slices_integrated covers it; or decide fail"
	}
	return "decide fail and restart with a corrected decomposition: retrying cannot succeed under this plan and runner kind"
}

// amendAncestorMessage is the static message for each ancestor 409 reason.
func amendAncestorMessage(reason string) string {
	const suffix = " acknowledge_owner_unresolved does not relax this."
	switch reason {
	case amendAncestorNoWaveGate:
		return "a submitted path is owned by a depends_on ancestor slice, but this child's dispatch does not pass through the host wave gate, so nothing proves that ancestor's work is in this child's base." + suffix
	case amendAncestorRunNotFound:
		return "a submitted path is owned by a depends_on ancestor slice that has no identifiable child run, so its delivery into this child's base cannot be proven." + suffix
	case amendAncestorNoImplement:
		return "a submitted path is owned by a depends_on ancestor slice whose run has no implement stage, so its delivery into this child's base cannot be proven." + suffix
	case amendAncestorNotIntegrated:
		return "a submitted path is owned by a depends_on ancestor slice that the parent's newest slices_integrated record does not prove merged onto a consolidated branch, so it is not proven in this child's base." + suffix
	}
	return "a submitted path is owned by a depends_on ancestor slice that has not delivered: its run and implement stage have not both succeeded, so its work is not in this child's base." + suffix
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
//	(a) validate paths         — 400, nothing touched
//	(b) coverage check         — 409, nothing touched
//	(c) in-flight row lookup   — 500 amend_unrecorded, READ-ONLY, nothing touched
//	(d) per-stage amend cap    — 409, nothing touched
//	(e) slice-ownership guard  — 409 with a refused row, or 500, nothing else touched
//	(f) owner + ancestor proof — 409 or 500, nothing touched
//	(g) pre-approved row       — 500 amend_unrecorded, stage left PARKED
//	(h) amended audit entry    — 500 amend_unrecorded, stage left PARKED
//	(i) resume to pending      — 500 amend_resume_failed, stage left PARKED
//	(j) orchestrator advance    — best-effort WARN (mirrors both existing arms)
//
// Steps (a)-(f) are the refusals that touch neither row, stage nor park; a stage
// whose widening the chain never recorded is never dispatched. Every step from
// (g) on is RETRY-SAFE: re-POSTing the same amend reuses this episode's row (g),
// re-appends an entry keyed to the same amendment_id so the cap is unmoved (h),
// and re-attempts the resume (i).
//
// (e) runs BEFORE (f) so a non-ancestor owner hits the ownership 409 and its
// audit row whatever that sibling's state, and never reaches
// amend_refused_owner_slice_active. acknowledge_owner_unresolved relaxes neither.
//
// (c) runs BEFORE the cap deliberately. A resume failure on the LAST permitted
// amend has already recorded that amendment against the cap, so a cap check that
// ignored the in-flight row would refuse the retry amend_budget_exhausted and
// strand the stage parked with no admissible decision left. The cap therefore
// admits a request that resolves to an amendment it has ALREADY counted — the
// retry adds no new amendment — while a request resolving to no such row is
// capped exactly as before.
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

	// (c) IN-FLIGHT ROW LOOKUP — read-only. Which pre-approved row, if any, THIS
	// exact decision already minted within THIS park episode. Keyed on the episode
	// so a fresh park never adopts a completed episode's row (and so cannot amend
	// forever under one amendment_id).
	episode := amendEpisodeKey(park)
	markerReason := operatorAmendMarkerReason(episode, reqBody.Reason)
	var inflight *scopeamendment.Amendment
	if episode != "" {
		inflight, err = s.findOperatorAmendRow(ctx, runID, stage.ID, paths, markerReason)
		if err != nil {
			// Refuse rather than risk a duplicate: without the prior rows we cannot
			// tell a fresh amend from a retry, and guessing wrong drains the agent's
			// budget silently.
			s.writeError(w, r, http.StatusInternalServerError, "amend_unrecorded",
				"the scope widening could not be recorded; the stage is left parked — re-POST the decision",
				map[string]any{internalCauseKey: err.Error()})
			return
		}
	}

	// (d) PER-STAGE AMEND CAP.
	usage, err := s.scopeCompletenessAmendUsage(ctx, runID, stage.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"count prior scope-completeness amendments failed",
			map[string]any{internalCauseKey: err.Error()})
		return
	}
	// A retry of an amendment the cap ALREADY counted adds no new amendment, so it
	// stays admissible at the boundary — otherwise a resume failure on the last
	// permitted amend would strand the stage parked with no decision left.
	if usage.count >= maxScopeCompletenessAmendsPerStage && !usage.records(inflight) {
		s.writeError(w, r, http.StatusConflict, "amend_budget_exhausted",
			"this stage has exhausted its amend-and-resume budget; the remaining resolution is \"fail\" plus a corrected decomposition",
			map[string]any{"max": maxScopeCompletenessAmendsPerStage, "used": usage.count})
		return
	}

	// (e) SLICE-OWNERSHIP GUARD (#4798). The run row comes from
	// resolveParkedScopeStage, which already refused a GetRun failure —
	// re-fetching it here would add a round-trip whose error branch no request
	// could reach.
	ownership, refused := s.refuseSliceOwnedPreApproval(w, r, runRow, stage.ID, paths, sliceOwnershipPhaseAmend, reqBody.Reason, decidedBy)
	if refused {
		return
	}

	// (f) OWNER-SLICE GUARD AND ANCESTOR-DELIVERY PROOF.
	guard, refusal := s.resolveAmendOwnerGuard(ctx, runRow, park, paths, ownership)
	if refusal != nil {
		s.writeAmendOwnerRefusal(w, r, refusal)
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

	// (g) The pre-approved #961 row — the SAME create+auto-approve mechanism
	// recover.go uses, so no prompt.go fold change is needed: the row is already
	// APPROVED, and resolveApprovedScopeAmendmentEntries filters on status alone
	// with no origin discriminator.
	amendmentID, err := s.resolveOrCreateOperatorAmendRow(ctx, inflight, runID, stage.ID, paths,
		markerReason, decidedBy)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "amend_unrecorded",
			"the scope widening could not be recorded; the stage is left parked — re-POST the decision",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	// (h) The amended audit entry. REFUSED on failure for the same reason the
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

	// (i) Pending, NOT Running — the #2501 reason: host_dispatch.go's admission
	// switch accepts only {pending, awaiting_host_dispatch}.
	if _, err := s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStatePending, nil); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "amend_resume_failed",
			"the widening was recorded but the parked stage could not be resumed; the stage is left parked — re-POST the decision (the amendment row is reused, not duplicated)",
			map[string]any{internalCauseKey: err.Error()})
		return
	}

	// (j) Best-effort advance, mirroring both existing arms.
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

// scopeAmendUsage is what this stage has already spent against the per-stage
// amend cap: how many DISTINCT amendments it has recorded, and WHICH — the
// second half being what lets a retry of an already-counted amendment stay
// admissible at the cap boundary.
type scopeAmendUsage struct {
	count int
	ids   map[string]struct{}
}

// records reports whether the cap has ALREADY counted this row's amendment, i.e.
// whether re-POSTing it would add a new amendment (it would not).
func (u scopeAmendUsage) records(a *scopeamendment.Amendment) bool {
	if a == nil {
		return false
	}
	_, ok := u.ids[a.ID.String()]
	return ok
}

// countScopeCompletenessAmends counts the DISTINCT amendments already applied to
// this stage — see maxScopeCompletenessAmendsPerStage for why the key is
// amendment_id and not the raw entry count.
func (s *Server) countScopeCompletenessAmends(ctx context.Context, runID, stageID uuid.UUID) (int, error) {
	usage, err := s.scopeCompletenessAmendUsage(ctx, runID, stageID)
	return usage.count, err
}

// scopeCompletenessAmendUsage is countScopeCompletenessAmends plus the identity
// of each counted amendment.
func (s *Server) scopeCompletenessAmendUsage(ctx context.Context, runID, stageID uuid.UUID) (scopeAmendUsage, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryScopeCompletenessAmended)
	if err != nil {
		return scopeAmendUsage{}, err
	}
	usage := scopeAmendUsage{ids: make(map[string]struct{}, len(entries))}
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
			usage.count++
			continue
		}
		if _, dup := usage.ids[payload.AmendmentID]; dup {
			continue
		}
		usage.ids[payload.AmendmentID] = struct{}{}
		usage.count++
	}
	return usage, nil
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
// submitted paths, on a path set refuseSliceOwnedPreApproval has already cleared
// of plan-declared non-ancestor owners.
//
// The contract is plan-first (#4798):
//
//   - A path the plan declares takes its owners from ownership.attributedOwners
//     and ignores the park, so a missing or nil park attribution cannot create an
//     acknowledgeable state. Every owner is evaluated, because directory
//     containment can give one path two ancestors.
//   - The own slice resolves with no read: a slice cannot conflict with itself.
//   - An ancestor, whether the plan or the park attributed it, must have PROVEN
//     delivery into this child's base (proveAncestorDelivered). A failed proof
//     returns at once, so it wins over any unresolved path in the set regardless
//     of order, and never enters `unresolved`.
//   - Only a path the plan does NOT declare can reach `unresolved`, the sole input
//     to acknowledge_owner_unresolved: no park entry, a nil park index, or the
//     legacy branch below for a park-attributed non-ancestor.
//
// THE LEGACY BRANCH. An amend hands a second slice permission to edit a file a
// sibling owns, which is safe ONLY while that sibling has not yet begun its
// implement pass: from Stage.StartedAt onwards two branches can carry divergent
// edits to one file and consolidation would conflict. StartedAt is the sound
// proxy because stages.started_at is written under COALESCE(started_at, $3) and
// never overwritten (see scope_amendment.go::amendmentDeadlineRemaining), so
// non-nil means an agent pass began. Its documented caveat — a re-spawned
// stage's started_at is CUMULATIVE — can only make this guard MORE conservative.
// This branch needs a plan revised under an in-flight fan-out, the anomaly
// guardDecompositionWaveOrder's comment documents, because park attribution comes
// from the same plan as the ownership check.
//
// A run that is not a decomposition child (DecomposedFrom == nil) has no sibling
// slice, so nothing is uncertain and nothing is read.
func (s *Server) resolveAmendOwnerGuard(ctx context.Context, runRow *run.Run,
	park *run.ScopeCompletenessPark, paths []scopeamendment.PathEntry, ownership *sliceOwnershipCheck) (amendOwnerGuardResult, *amendOwnerActiveRefusal) {
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

	// A list failure is decided per path: it refuses an ancestor's proof and is
	// merely unresolved for a legacy attribution, and a request naming only the
	// run's own slice is unaffected.
	siblings, listErr := s.listDecomposedSiblings(ctx, *runRow.DecomposedFrom)
	if listErr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"scope-completeness amend: sibling slice lookup failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", listErr.Error()))
	}
	proof := &amendAncestorProof{siblings: siblings, listErr: listErr, proven: map[int]bool{}}

	for _, p := range paths {
		owners, planned := ownership.attributedOwners(p.Path)
		if !planned {
			owner, ok := byPath[p.Path]
			if !ok {
				out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedNoEntry})
				continue
			}
			if owner.SliceIndex == nil {
				out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedNoSliceIndex})
				continue
			}
			owners = []run.BuildRequiredOwner{owner}
		}
		for _, owner := range owners {
			idx := *owner.SliceIndex
			if runRow.SliceIndex != nil && *runRow.SliceIndex == idx {
				out.resolved = append(out.resolved, owner)
				continue
			}
			if ownership.isAncestorSlice(idx) {
				if refusal := s.proveAncestorDelivered(ctx, runRow, p.Path, owner, proof); refusal != nil {
					return out, refusal
				}
				out.resolved = append(out.resolved, owner)
				continue
			}
			if listErr != nil {
				out.unresolved = append(out.unresolved, amendUnresolvedOwner{Path: p.Path, Reason: amendOwnerUnresolvedSiblingLookup})
				continue
			}
			sibling := siblingForSlice(siblings, runRow.ID, idx)
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
					Path:           p.Path,
					SliceIndex:     idx,
					SliceTitle:     owner.SliceTitle,
					SiblingRun:     sibling.ID.String(),
					SiblingStat:    string(implement.State),
					SiblingRunStat: string(sibling.State),
				}
			}
			out.resolved = append(out.resolved, owner)
		}
	}
	return out, nil
}

// amendAncestorProof holds the reads one amend decision shares, so each is made
// at most once however many ancestor paths it names. A failed proof ends the
// decision, so only successes need remembering.
type amendAncestorProof struct {
	siblings []*run.Run
	listErr  error

	waveGateApplies bool
	integrationRead bool
	branch          string
	integrated      []string
	proven          map[int]bool
}

// childWaveGateApplies reports whether this child's dispatch goes through
// handleHostDispatchStage, the only caller of guardDecompositionWaveOrder and
// resolveDependentChildBase. It mirrors runnerbackend.Resolver's routing: a
// non-host kind fires remotely; a locked host child is host-dispatched; an
// un-resolved host hint defers to the parent's lock, because a child is minted
// un-resolved with RunnerKind COPIED (#1980) and a parent locked remote supersedes
// the hint. A parent that is itself un-resolved parks the child at
// awaiting_host_dispatch, which is host dispatch too. A parked child may not be
// locked, because the lock comes from the signed trace manifest, so this cannot
// require the lock.
func (s *Server) childWaveGateApplies(ctx context.Context, child *run.Run) (bool, error) {
	hostDispatched, known := runnerbackend.KindHostDispatched(child.RunnerKind)
	if !known || !hostDispatched {
		return false, nil
	}
	if child.RunnerKindResolved {
		return true, nil
	}
	parent, err := s.cfg.RunRepo.GetRun(ctx, *child.DecomposedFrom)
	if err != nil {
		return false, err
	}
	if parent.RunnerKindResolved {
		return parent.RunnerKind == run.RunnerKindLocal, nil
	}
	return true, nil
}

// proveAncestorDelivered is the Captain's ruling on #4798, "Require proven ancestor
// delivery": a plan-declared ancestor's path is allowed only when that ancestor's
// work is proven in this child's base, and every failure of the proof refuses.
//
// No per-child base is persisted, so the proof is built from three facts. ROUTING:
// this child's dispatch goes through handleHostDispatchStage, which admits a
// dependent child only after guardDecompositionWaveOrder and
// resolveDependentChildBase, and the amend's own re-dispatch passes both gates
// again. STATE: the ancestor's run and implement stage succeeded, and succeeded is
// absorbing (run.TestRunSucceededIsAbsorbing). INTEGRATION: the parent's newest
// slices_integrated names a branch and covers the ancestor, the same reader and
// predicate the wave gate uses; IntegrateSlices merges every succeeded slice on
// each pass, so the transitive closure is covered too.
//
// It returns nil when proven. AuditRepo cannot be nil here because
// handleDecideScopeCompleteness answers 503 first.
func (s *Server) proveAncestorDelivered(ctx context.Context, child *run.Run, path string, owner run.BuildRequiredOwner, proof *amendAncestorProof) *amendOwnerActiveRefusal {
	idx := *owner.SliceIndex
	if proof.proven[idx] {
		return nil
	}
	refuse := func(reason string, retryable bool) *amendOwnerActiveRefusal {
		return &amendOwnerActiveRefusal{
			Path: path, SliceIndex: idx, SliceTitle: owner.SliceTitle,
			Ancestor: true, Reason: reason, Retryable: retryable,
		}
	}
	readFailed := func(reason string, err error) *amendOwnerActiveRefusal {
		r := refuse(reason, true)
		r.ReadFailed = true
		r.cause = err.Error()
		return r
	}

	if !proof.waveGateApplies {
		applies, err := s.childWaveGateApplies(ctx, child)
		if err != nil {
			return readFailed(amendAncestorChannelUnreadable, err)
		}
		if !applies {
			return refuse(amendAncestorNoWaveGate, false)
		}
		proof.waveGateApplies = true
	}

	if proof.listErr != nil {
		return readFailed(amendAncestorSiblingsUnreadable, proof.listErr)
	}
	sibling := siblingForSlice(proof.siblings, child.ID, idx)
	if sibling == nil {
		r := refuse(amendAncestorRunNotFound, false)
		r.SiblingRunStat = notMintedState
		return r
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, sibling.ID)
	if err != nil {
		return readFailed(amendAncestorStagesUnreadable, err)
	}
	withSibling := func(r *amendOwnerActiveRefusal, implementState string) *amendOwnerActiveRefusal {
		r.SiblingRun = sibling.ID.String()
		r.SiblingRunStat = string(sibling.State)
		r.SiblingStat = implementState
		return r
	}
	implement := implementStageOf(stages)
	if implement == nil {
		return withSibling(refuse(amendAncestorNoImplement, false), "")
	}
	if sibling.State != run.StateSucceeded || implement.State != run.StageStateSucceeded {
		return withSibling(refuse(amendAncestorNotDelivered, true), string(implement.State))
	}

	if !proof.integrationRead {
		branch, integrated, err := s.latestSlicesIntegrated(ctx, *child.DecomposedFrom)
		if err != nil {
			return readFailed(amendAncestorIntegrationUnreadable, err)
		}
		proof.branch, proof.integrated, proof.integrationRead = branch, integrated, true
	}
	branch, integrated := proof.branch, proof.integrated
	covered, _ := wavecoverage.Covered([]int{idx}, map[int]string{idx: sibling.ID.String()}, integrated)
	if !covered || branch == "" {
		r := withSibling(refuse(amendAncestorNotIntegrated, true), string(implement.State))
		present := branch != ""
		r.branchPresent = &present
		return r
	}
	proof.proven[idx] = true
	return nil
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

// findOperatorAmendRow is the READ-ONLY half of the idempotence: the row THIS
// decision already minted within THIS park episode, or nil.
//
// The reuse key is (stage, marker reason INCLUDING the episode key, identical
// path set), which is exactly what a re-POST of the same decision within one
// park presents — and, because of the episode key, exactly what a SECOND park
// submitting byte-identical input does NOT present.
func (s *Server) findOperatorAmendRow(ctx context.Context, runID, stageID uuid.UUID,
	paths []scopeamendment.PathEntry, markerReason string) (*scopeamendment.Amendment, error) {
	existing, err := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, a := range existing {
		if a == nil || a.StageID != stageID || a.Reason != markerReason || !samePathSet(a.Paths, paths) {
			continue
		}
		if a.Status == scopeamendment.StatusApproved || a.Status == scopeamendment.StatusPending {
			return a, nil
		}
	}
	return nil, nil
}

// resolveOrCreateOperatorAmendRow returns the pre-approved #961 row carrying the
// widening: the in-flight row findOperatorAmendRow resolved, or a freshly minted
// one when there is none.
//
// IDEMPOTENT BY REUSE (binding condition 2). The row, the audit entry and the
// stage transition are three separate writes with no shared transaction, so a
// failure at either of the later two leaves an orphaned APPROVED row behind. A
// duplicate row is inert for the effective scope (foldScopeEntries dedupes by
// path) but NOT for the budget, which counts ROWS — so a naive retry would eat
// one of the re-run agent's two #961 slots per failed cycle. A PENDING orphan
// (Create succeeded, the auto-approve Decide did not) is COMPLETED rather than
// duplicated.
//
// A retry that changes the paths or the reason is a DIFFERENT amendment and
// correctly mints its own row; the orphan then stays and does consume a slot.
// That residual is surfaced, not hidden: the response's
// agent_amendment_budget_remaining reports the true remaining budget.
func (s *Server) resolveOrCreateOperatorAmendRow(ctx context.Context, inflight *scopeamendment.Amendment,
	runID, stageID uuid.UUID, paths []scopeamendment.PathEntry, markerReason, decidedBy string) (uuid.UUID, error) {
	if inflight != nil {
		if inflight.Status == scopeamendment.StatusApproved {
			return inflight.ID, nil
		}
		if err := s.approveOperatorAmendRow(ctx, inflight.ID, decidedBy); err != nil {
			return uuid.Nil, err
		}
		return inflight.ID, nil
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
	if err := s.approveOperatorAmendRow(ctx, amendment.ID, decidedBy); err != nil {
		return uuid.Nil, err
	}
	return amendment.ID, nil
}

// approveOperatorAmendRow auto-approves the operator's own row: the amend
// decision IS the approval, so the #961 row is never left for a second gate.
func (s *Server) approveOperatorAmendRow(ctx context.Context, id uuid.UUID, decidedBy string) error {
	_, err := s.cfg.ScopeAmendmentRepo.Decide(ctx, scopeamendment.DecideParams{
		ID:        id,
		Status:    scopeamendment.StatusApproved,
		Reason:    "pre-approved by the amending operator",
		DecidedBy: decidedBy,
	})
	return err
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
