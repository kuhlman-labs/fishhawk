package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
)

// nearCapMargin is the policy threshold (#2492): a plan whose scanned
// scope.files count leaves this many files of headroom or fewer under the
// resolved implement-stage max_files_changed cap (but is NOT itself over cap)
// fires the near-cap advisory. It is a POLICY value pinned by
// TestRunPlanWarnings_NearCap_ThresholdBoundary (fires at headroom 0..3, silent
// at 4+), NOT a derived one — it is deliberately small so a plan with real
// headroom never fires and changing it requires editing the test that states the
// intent. Taken directly from the issue's proposed ~3-file margin.
const nearCapMargin = 3

// categoryPlanWarnings is the audit-log category for the entry
// runPlanWarnings writes when plan.Warnings() fires at least one advisory
// for an uploaded plan (#1684). Unlike plan_scope_precheck/
// plan_surface_sweep/plan_test_sweep, this entry is written ONLY when
// Warnings() returns a non-empty slice — a warning-free plan gets no
// entry, keeping TestShipPlan's happy-path audit-count assertion green
// (binding condition 3).
const categoryPlanWarnings = "plan_warnings"

// PlanWarningsPayload is the audit-payload shape for a plan_warnings entry
// (#1684). Warnings mirrors plan.Warnings()'s return value verbatim.
type PlanWarningsPayload struct {
	Warnings []string `json:"warnings"`
}

// runPlanWarnings evaluates an uploaded plan with plan.Warnings() —
// notably the all-empty-depends_on decomposition advisory (#1679/#1680),
// plus the pre-existing sub-plan runtime-sum and expensive-gate-vs-budget
// advisories — and, when it returns at least one warning, records an
// advisory plan_warnings audit entry (#1684). This gives plan.Warnings()
// its first production caller, closing the gap where the decomposition
// safety net computed a result nobody ever read.
//
// Advisory-only and fail-open: it guards only on AuditRepo for the
// plan.Warnings() advisories (which depend only on the parsed plan itself),
// and a plan.Parse error or an audit-append failure WARN-logs and continues
// rather than blocking or unwinding the upload. It never transitions or fails
// the plan stage.
//
// SERVER-AUTHORITATIVE over-cap advisory (#2053): it ALSO appends a
// deterministic over-cap advisory when the resolved implement-stage
// max_files_changed cap is > 0 and len(scope.files) > cap. This computation is
// the SINGLE SOURCE OF TRUTH for the over-cap condition — it reads ONLY the
// scanned file count and the resolved cap and MUST NOT read parsedPlan.OverCap
// (the planner's self-declaration hint). It is the guarantee that no monolithic
// over-cap plan silently passes the plan gate; downstream E50 slices key on it.
// Resolving the cap adds a RunRepo dependency the plan.Warnings() advisories do
// not need; that leg fails open independently (nil RunRepo, GetRun error, no
// spec/implement stage, or cap unresolvable → over-cap advisory skipped, plan
// settle never blocked), so the plan.Warnings() advisories still fire even when
// the cap cannot be resolved.
//
// Returns the computed payload so a future caller can thread it into the
// plan-review prompt's gate-evidence section (not wired in this slice); nil when
// neither plan.Warnings() nor the over-cap check found anything to report, or on
// any fail-open path.
func (s *Server) runPlanWarnings(ctx context.Context, runID, stageID uuid.UUID, planBody []byte) *PlanWarningsPayload {
	if s.cfg.AuditRepo == nil {
		return nil
	}

	// Validation already passed in handleShipPlan; decode with json.Unmarshal
	// (schema-only bytes, NOT plan.Parse) so the advisories stay independent of
	// the semanticCheck over_cap ⇒ split_proposal coupling (#2055): an over-cap
	// over_cap:true plan that omits split_proposal would fail plan.Parse, which
	// would SUPPRESS the count-derived over-cap advisory for exactly the flag
	// state #2053 requires it to fire in — reversing #2053's flag-independence
	// guarantee. Decoding here never runs semanticCheck; the authoritative gate
	// (handleShipPlan overCapSplitRejection) owns the reject. A json.Unmarshal
	// failure on schema-valid bytes is an internal inconsistency — log and skip
	// rather than block (fail-open, unchanged).
	var parsedPlan plan.Plan
	if err := json.Unmarshal(planBody, &parsedPlan); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan warnings: decode plan failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	warnings := plan.Warnings(&parsedPlan)

	// SERVER-AUTHORITATIVE over-cap advisory (#2053): count-derived, computed
	// independently of the planner's over_cap self-declaration. Appended before
	// the emptiness check so an otherwise warning-free over-cap plan still
	// records a plan_warnings entry that surfaces through fishhawk_get_plan.
	if w := s.overCapWarning(ctx, runID, &parsedPlan); w != "" {
		warnings = append(warnings, w)
	}

	// NEAR-cap advisory (#2492), appended immediately AFTER the count-derived
	// over-cap advisory and BEFORE phaseCapWarnings/irreducibleWarning: it is the
	// same count-derived cap family and belongs adjacent to it, and its position
	// keeps the #2053 ordering guarantee intact — the over-cap advisory is still
	// emitted first and unchanged. nearCapWarning returns "" whenever the plan is
	// already OVER cap (the over-cap advisory owns that case), so this append is a
	// no-op for every over-cap plan and cannot alter the existing over-cap/
	// irreducible/phase-cap warnings slice.
	if w := s.nearCapWarning(ctx, runID, &parsedPlan); w != "" {
		warnings = append(warnings, w)
	}

	// #2412 advisories, appended AFTER the count-derived over-cap advisory so the
	// count advisory is emitted FIRST and unchanged — the ordering is the
	// observable proof that irreducible/phase-cap advisories do not SUPPRESS the
	// count-derived over-cap advisory. Both legs fail open on an unresolved cap.
	warnings = append(warnings, s.phaseCapWarnings(ctx, runID, &parsedPlan)...)
	if w := s.irreducibleWarning(ctx, runID, &parsedPlan); w != "" {
		warnings = append(warnings, w)
	}

	if len(warnings) == 0 {
		return nil
	}

	result := &PlanWarningsPayload{Warnings: warnings}
	payload, _ := json.Marshal(result)
	systemKind := audit.ActorKind("system")
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  categoryPlanWarnings,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan warnings: append audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
	return result
}

// overCapWarning computes the SERVER-AUTHORITATIVE, count-derived over-cap
// advisory string for a parsed plan (#2053), or "" when the plan is not over
// cap or the cap cannot be resolved. It is the single source of truth for the
// over-cap condition: the over-cap decision reads ONLY len(scope.files) and the
// resolved implement-stage max_files_changed cap and MUST NOT consult
// parsedPlan.OverCap (the planner's self-declaration hint) — so an over-cap plan
// fires the advisory whether over_cap is omitted, false, or true, and an
// under-cap plan never fires it even when over_cap is true.
//
// Fail-open on every leg (nil RunRepo, GetRun error, no spec/implement stage, or
// an unresolved / zero cap): return "" so the over-cap advisory is skipped and
// the plan settle is never blocked. The cap resolution reuses
// resolveImplementConstraints, the same helper runScopePrecheck uses, so the
// plan-gate advisory and the scope pre-check agree on the resolved cap.
func (s *Server) overCapWarning(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	count, capLimit, over, ok := s.overCapByCount(ctx, runID, parsedPlan)
	if !ok || !over {
		return ""
	}
	return fmt.Sprintf(
		"plan scope declares %d files, exceeding the implement-stage max_files_changed cap of %d — "+
			"narrow the scope or split the work into a decomposition before approving.",
		count, capLimit,
	)
}

// nearCapWarning computes the NEAR-cap plan-gate advisory (#2492) for a parsed
// plan, or "" when the plan is not near cap, is already over cap, or the cap
// cannot be resolved. It makes the SHARED whole-plan budget visible at the one
// gate where the operator can act on it: a plan whose scope.files lands within
// nearCapMargin of the resolved implement-stage max_files_changed cap is told
// how many files of headroom remain and that spending that headroom (via an
// approval-time add_scope_files or a mid-stage scope amendment) leaves a correct
// mid-stage fix no path in without a re-plan or a governed cap raise.
//
// It routes through the SAME shared overCapByCount cap resolution as the over-cap
// advisory and the authoritative reject, so the three can never disagree on the
// resolved cap. The three guards are ordered so mutual exclusion is STRUCTURAL,
// not a second check:
//
//	(a) !ok    — the cap is unresolved (nil RunRepo, GetRun error, no spec/
//	             implement stage, or a zero/unresolved cap): fail open exactly as
//	             every sibling advisory does.
//	(b) over   — the plan is ALREADY over cap: overCapWarning/overCapSplitRejection
//	             own that case, so returning "" here makes the near-cap and over-cap
//	             advisories mutually exclusive BY CONSTRUCTION.
//	(c) capLimit-count > nearCapMargin — the plan has real headroom: no advisory.
//
// Otherwise (count <= capLimit AND capLimit-count <= nearCapMargin) it renders the
// advisory. It reads ONLY the scanned count and resolved cap via overCapByCount —
// never parsedPlan.OverCap.
//
// DECOMPOSITION-AWARE: when the plan carries a decomposition of >= 2 sub-plans, a
// second sentence names the slice count and states plainly that all N slices draw
// against this ONE whole-plan budget — the 'more prominently for a decomposed
// plan' half of the issue's done-means. A single-slice or absent decomposition
// renders only the base sentence.
func (s *Server) nearCapWarning(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	count, capLimit, over, ok := s.overCapByCount(ctx, runID, parsedPlan)
	if !ok {
		return ""
	}
	if over {
		return ""
	}
	headroom := capLimit - count
	if headroom > nearCapMargin {
		return ""
	}
	msg := fmt.Sprintf(
		"plan scope declares %d files against the implement-stage max_files_changed cap of %d — only %d file(s) of headroom remain. "+
			"Once that headroom is spent, the plan-approval scope-cap gate and the mid-stage scope-amendment headroom check refuse any "+
			"further file, so a correct mid-stage fix that needs an un-scoped file has no path in without a re-plan or a governed cap raise.",
		count, capLimit, headroom,
	)
	slices := 0
	if parsedPlan.Decomposition != nil {
		slices = len(parsedPlan.Decomposition.SubPlans)
	}
	if slices >= 2 {
		msg += fmt.Sprintf(
			" This plan is decomposed into %d slices that ALL draw against this ONE whole-plan budget, so the headroom is shared — "+
				"the first slice to need an extra file spends it for every other slice.",
			slices,
		)
	}
	return msg
}

// phaseCapWarnings emits one plan-gate advisory per split_proposal PHASE whose
// OWN declared scope.files count exceeds the resolved implement cap (#2412) —
// the defect #2410 reports, where a split's lead phase is itself over cap and
// would fail category-B in its own implement stage. It resolves the cap via
// overCapByCount (so the gate advisory, the count reject, and this advisory
// never disagree on the resolved cap — take capLimit/ok, ignore over) and calls
// splitfiling.PhaseCapViolations. Each advisory reuses PhaseCapViolation.String()
// so the plan-gate advisory, the refusal audit payload, and the parent comment
// share ONE phrasing.
//
// Fail-open: nil when the plan carries no split_proposal or the cap is
// unresolved (ok=false) — no advisory, plan settle never blocked.
func (s *Server) phaseCapWarnings(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) []string {
	if parsedPlan.SplitProposal == nil {
		return nil
	}
	_, capLimit, _, ok := s.overCapByCount(ctx, runID, parsedPlan)
	if !ok {
		return nil
	}
	violations := splitfiling.PhaseCapViolations(*parsedPlan.SplitProposal, capLimit)
	if len(violations) == 0 {
		return nil
	}
	out := make([]string, 0, len(violations))
	for _, v := range violations {
		out = append(out, v.String()+
			" — a split exists to bring EVERY phase under the cap, so re-slice this phase or declare the change irreducible.")
	}
	return out
}

// irreducibleWarning surfaces the planner's irreducible rationale to the operator
// as a CHALLENGEABLE claim (#2412). It fires ONLY when the plan is over cap BY
// COUNT (the same count-derived condition the reject relaxation keys on) AND
// carries a well-formed Irreducible.Declared() declaration — so it never fires on
// an under-cap plan (the #2055 flag-independence + under-cap-unaffected
// guarantee: an under-cap plan returns before the declaration is read). It states
// plainly that the declaration does NOT make the change landable and that landing
// it needs a governed max_files_changed raise, because implement re-checks the
// real diff. Returns "" when not over cap, the cap is unresolved, or no
// declaration is present (fail-open — plan settle never blocked).
func (s *Server) irreducibleWarning(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	_, capLimit, over, ok := s.overCapByCount(ctx, runID, parsedPlan)
	if !ok || !over {
		return ""
	}
	if !parsedPlan.Irreducible.Declared() {
		return ""
	}
	msg := fmt.Sprintf(
		"plan declares itself IRREDUCIBLE at %d files over the implement-stage max_files_changed cap of %d: %s",
		len(parsedPlan.Scope.Files), capLimit, parsedPlan.Irreducible.Rationale,
	)
	if basis := parsedPlan.Irreducible.AtomicityBasis; basis != "" {
		msg += fmt.Sprintf(" (atomicity basis: %s)", basis)
	}
	msg += ". This is a challengeable claim — verify the change genuinely cannot be phased into at-or-under-cap commits. " +
		"The declaration does NOT make the change landable: the implement stage re-checks max_files_changed against the real diff, " +
		"so approving this over-cap plan still needs a governed max_files_changed raise."
	return msg
}

// overCapByCount is the shared #2053 count determination (E50.3 refactor). It
// resolves the implement-stage max_files_changed cap and compares it against
// len(parsedPlan.Scope.Files), NEVER reading parsedPlan.OverCap — the planner's
// self-declaration hint plays no part in whether a plan is over cap. It returns
// count (scanned scope.files), capLimit (the resolved cap), over (count >
// capLimit against a positive cap), and ok (the cap resolved to a positive
// value).
//
// Fail-open on every leg (nil RunRepo, GetRun error, no spec/implement stage,
// or a zero/unresolved cap): ok=false and over=false, so an unresolved cap can
// never be treated as over-cap. Both overCapWarning (advisory) and
// overCapSplitRejection (authoritative reject) route through this one function,
// so the plan-gate advisory and the count-derived reject agree on the resolved
// cap by construction — no duplicated cap logic. The cap resolution reuses
// resolveImplementConstraints, the same helper runScopePrecheck uses.
func (s *Server) overCapByCount(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) (count, capLimit int, over, ok bool) {
	count = len(parsedPlan.Scope.Files)
	if s.cfg.RunRepo == nil {
		return count, 0, false, false
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan warnings: get run for over-cap check failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return count, 0, false, false
	}
	constraints, _, resolved := s.resolveImplementConstraints(ctx, runRow)
	if !resolved || constraints.MaxFilesChanged <= 0 {
		return count, 0, false, false
	}
	return count, constraints.MaxFilesChanged, count > constraints.MaxFilesChanged, true
}

// overCapSplitRejection is the SERVER-AUTHORITATIVE, count-derived over-cap
// REJECT reason (#2055, E50.3) — the E50 keystone. It returns a clear,
// actionable reason when the plan is over cap BY COUNT (len(scope.files) >
// resolved cap) AND carries no split_proposal, and "" otherwise. Like the
// sibling advisory it reads ONLY the file count and the resolved cap via
// overCapByCount and MUST NOT consult parsedPlan.OverCap — so an over-cap-by-
// count monolith lacking a split is rejected whether over_cap is omitted,
// false, or true, and an over-cap plan that carries a split_proposal is
// accepted (structural validity of that split is the plan.Parse semanticCheck's
// job, an additional in-artifact defensive layer).
//
// Fail-open on every leg exactly like overCapWarning: overCapByCount returns
// ok=false (nil RunRepo, GetRun error, no spec/implement stage, unresolved/zero
// cap) → "" so an unresolved cap never spuriously blocks a plan. It reuses
// overCapByCount (and thus resolveImplementConstraints) so the reject and the
// advisory share one cap resolution with no duplicated logic.
//
// IRREDUCIBLE WIDEN-ONLY relaxation (#2412). After the split_proposal
// short-circuit, a well-formed Irreducible.Declared() ALSO converts the reject
// into "" — the planner has honestly DECLINED a split for a compile-atomic
// change rather than fabricating an invalid one. This does NOT violate the #2055
// contract, and the guarantee is POSITIONAL, not merely intended: the function
// still reads ONLY the file count and resolved cap to decide the plan IS over
// cap (the `!ok || !over` return precedes everything, and it still never reads
// over_cap); `irreducible` is consulted STRICTLY AFTER that decision and ONLY to
// widen a hard reject into an operator-decidable advisory (irreducibleWarning +
// the count advisory both still fire in runPlanWarnings). Because the over-cap
// decision is made first, an under-cap plan returns before the Irreducible field
// is ever read, so no under-cap plan can change behaviour — the count-blind
// rejection #2055 banned (reading a field to REJECT a plan the count says is
// fine) is structurally impossible here.
//
// Deliberately NOT hard-rejected here: an over-cap PHASE. An over-cap phase is
// surfaced as a gate advisory (phaseCapWarnings) and hard-refused at FILING time
// (fileSplitProposalChildren), which satisfies the issue's "refuses (or flags
// loudly)" without risking a reject loop for a planner that cannot re-slice and
// has not reached for irreducible.
func (s *Server) overCapSplitRejection(ctx context.Context, runID uuid.UUID, parsedPlan *plan.Plan) string {
	count, capLimit, over, ok := s.overCapByCount(ctx, runID, parsedPlan)
	if !ok || !over {
		return ""
	}
	if parsedPlan.SplitProposal != nil {
		return ""
	}
	if parsedPlan.Irreducible.Declared() {
		return ""
	}
	return fmt.Sprintf(
		"plan scope declares %d files, exceeding the implement-stage max_files_changed cap of %d, "+
			"and carries neither a split_proposal nor an irreducible declaration — split the work into an "+
			"expand->migrate->contract split_proposal (each phase at or under the cap), or if the change is "+
			"genuinely compile-atomic declare it irreducible with a rationale, before approving.",
		count, capLimit,
	)
}
