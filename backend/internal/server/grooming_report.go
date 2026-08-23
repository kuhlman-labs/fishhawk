package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// CategoryGroomingReportRecorded is the audit category appended when a
// grooming_report artifact is ingested (#2235). Registered in
// audit.KnownCategories so fishhawk_await_audit can arm on it.
const CategoryGroomingReportRecorded = "grooming_report_recorded"

// groomingReportSchemaVersion is the schema_version recorded for an ingested
// grooming_report (#2235). It mirrors the canonical
// docs/spec/grooming-report-v1.schema.json version token and the workflow-spec
// `schema:` a grooming_report-producing stage must declare.
const groomingReportSchemaVersion = plan.GroomingReportVersion

// groomingIngestMu serializes the grooming-report ingest CRITICAL SECTION —
// GetByHash, the audit-entry heal, Create, and the chained append — so the
// existence check and the writes it guards are one atomic step (#2235 fix-up).
//
// Without it the handler holds two separate check-then-act windows, and the
// runner retries identical POSTs:
//
//   - two concurrent first-POSTs both miss GetByHash, both Create, and both
//     append → two artifact rows and two audit entries for one report;
//   - a retry can slip between the original's Create and its AppendChained,
//     observe the artifact present and the chain silent, and heal — after which
//     the original appends its own entry → one artifact, two entries.
//
// ensureGovernanceAuditEntry's own lock closes only the heal-versus-heal race;
// it cannot see the create path, which never takes that lock. This one wraps
// both paths. Lock ordering is always groomingIngestMu → governanceHealMu (the
// heal is called from inside this section and never the reverse), so the pair
// cannot deadlock. Contention is negligible: one report per plan stage.
//
// RESIDUAL, stated rather than implied: this is a PROCESS-LOCAL lock, so two
// fishhawkd replicas ingesting the same report at the same instant can still
// double-write. Closing that needs DB-level dedup — a uniqueness constraint on
// (stage_id, content_hash) governing every artifact kind — which is a schema
// change beyond this artifact's slice; the same residual is documented on
// ensureGovernanceAuditEntry.
var groomingIngestMu sync.Mutex

// handleGroomingReport ingests a grooming_report artifact — the SECOND additive
// plan-stage sibling (#2235, ADR-065 §3) — shipped to POST
// /v0/runs/{run_id}/plan. A `plan`-typed PROPOSE stage running a backlog-grooming
// workflow emits it instead of a plan: a set of typed PROPOSALS (a rubric-cited
// ordering, duplicate candidates, hygiene defects, suggested depends_on edges,
// vision-drift flags, decomposition suggestions) a human decides on. Nothing here
// mutates a tracker.
//
// Unlike the clarification_request sibling, the report is persisted as an
// ARTIFACT row (kind grooming_report, admitted by migration 0073) with its
// content hash, because #2240's run-over-run churn guard reads the report body
// BY HASH; the audit entry carries the hash plus the cheap per-class entry
// counts rather than the whole document.
//
// Failure modes, each mapped to the category the runner needs:
//   - shipped from a non-`plan` stage → 400 grooming_report_stage_invalid,
//     stage fail-B (re-shipping the same bytes cannot help);
//   - schema / semantic violation     → 400 grooming_report_invalid, stage fail-B;
//   - artifact or audit storage error → 500 (runner retries; idempotent via
//     GetByHash, and the retry SELF-HEALS a missing audit entry).
//
// stage is the pre-fetched stage row (handleShipPlan confirmed it belongs to the
// run); body is the verified, size-capped request body.
func (s *Server) handleGroomingReport(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, body []byte) {
	// Stage-type guard. The workflow-spec validator already refuses to DECLARE
	// grooming_report off a plan stage; this is the runtime mirror for a report
	// shipped from a stage that never declared it. Category-B: the stage type is
	// a property of the run, so re-shipping the same bytes cannot help.
	if stage.Type != run.StageTypePlan {
		s.failGroomingStage(r, runID, stageID,
			"grooming_report_stage_invalid: shipped from a "+string(stage.Type)+" stage")
		s.writeError(w, r, http.StatusBadRequest, "grooming_report_stage_invalid",
			"grooming_report may only be shipped from a plan stage — the PROPOSE stage per ADR-067 §2",
			map[string]any{"stage_type": string(stage.Type)})
		return
	}

	// Validate against grooming-report-v1 plus the five semantic rules the
	// schema cannot express (report-wide id uniqueness, id class prefix, id
	// recomposition, rank permutation). An invalid report is the agent's bad
	// output: fail category-B and walk the run to terminal, exactly as the
	// plan-invalid and clarification-invalid paths do.
	report, err := plan.ParseGroomingReport(body)
	if err != nil {
		s.failGroomingStage(r, runID, stageID, "grooming_report_invalid: "+err.Error())
		s.writeError(w, r, http.StatusBadRequest, "grooming_report_invalid",
			"grooming_report does not validate against grooming-report-v1",
			map[string]any{"error": err.Error()})
		return
	}

	contentHash := sha256Hex(body)
	schemaVersion := groomingReportSchemaVersion
	counts := groomingEntryCounts(report)

	auditPayload := func(artifactID string) json.RawMessage {
		p, _ := json.Marshal(map[string]any{
			"run_id":         runID.String(),
			"stage_id":       stageID.String(),
			"artifact_id":    artifactID,
			"content_hash":   contentHash,
			"schema_version": schemaVersion,
			"size_bytes":     len(body),
			"entry_counts":   counts,
		})
		return p
	}
	systemKind := audit.ActorKind("system")
	appendEntry := func(artifactID string) error {
		_, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &stageID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryGroomingReportRecorded,
			ActorKind: &systemKind,
			Payload:   auditPayload(artifactID),
		})
		return aerr
	}

	// Idempotency: a runner retry re-POSTs the same report. Dedup on
	// (stage_id, content_hash) rather than inserting a duplicate row.
	//
	// The short-circuit MUST NOT return success on a gapped chain (#2235
	// condition F2). Create-then-append is two non-atomic steps: if Create
	// succeeded and AppendChained failed the handler 500'd with the report
	// already durable, and a naive retry would take this branch and report
	// success having never appended — leaving the report persisted and the audit
	// chain permanently silent about it. ensureGovernanceAuditEntry (#1396)
	// verifies the entry for THIS artifact exists and appends it when absent,
	// failing closed on a read error so a further retry can re-heal.
	//
	// Everything from here to the response is ONE critical section (see
	// groomingIngestMu): the existence check and the writes it authorizes must
	// not interleave with a concurrent identical POST.
	groomingIngestMu.Lock()
	defer groomingIngestMu.Unlock()

	if existing, gerr := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); gerr == nil {
		if _, herr := s.ensureGovernanceAuditEntry(r.Context(), runID,
			CategoryGroomingReportRecorded, existing.ID.String(), func() error {
				return appendEntry(existing.ID.String())
			}); herr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal grooming report audit entry failed", map[string]any{"error": herr.Error()})
			return
		}
		// Heal the churn verdict on the same retry, for the same reason: a
		// partial first write could have left the report durable and the guard's
		// verdict absent.
		if cerr := s.recordGroomingChurn(r, runID, stageID, existing.ID.String(), report); cerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal grooming churn audit entry failed", map[string]any{"error": cerr.Error()})
			return
		}
		// Settle on the retry too (#2837): a runner whose first POST 500'd AFTER
		// Create re-POSTs the same bytes and reaches this idempotent branch;
		// without the settle here that retry would re-strand the stage in
		// `running`. Same shared helper as the fresh-create path — a same-state
		// re-application is a valid no-op (ValidStageTransition returns true when
		// from == to), so a stage already settled on the first POST is unaffected.
		// The returned settled stage is unused here (grooming does not call
		// notifyPlanReadyIfReady — see the fresh-create path), so discard it.
		s.advancePlanStageTerminal(r, runID, stage)
		s.writeJSON(w, r, http.StatusOK, planResponse{
			ID:            existing.ID,
			StageID:       existing.StageID,
			ContentHash:   existing.ContentHash,
			SchemaVersion: schemaVersion,
			Idempotent:    true,
		})
		return
	} else if !errors.Is(gerr, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing grooming report failed", map[string]any{"error": gerr.Error()})
		return
	}

	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindGroomingReport,
		SchemaVersion: &schemaVersion,
		Content:       json.RawMessage(body),
		ContentHash:   contentHash,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create grooming report artifact failed", map[string]any{"error": err.Error()})
		return
	}

	// Audit. The chained append holds the runs row-lock so concurrent uploads
	// can't fork the hash chain. A failure here leaves an artifact with no
	// governance record — surface 500 so the runner retries; the GetByHash
	// branch above then HEALS the gap rather than papering over it.
	if err := appendEntry(created.ID.String()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append grooming report audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// The CHURN GUARD (E54.8 / #2240) runs here, on the real ingest path, and
	// its outcome — including AC1's visible "no changes proposed" — is written
	// to the audit chain. Same fail-closed contract as the report entry above:
	// a failure 500s so the runner retries, and the retry HEALS it, because a
	// guard whose verdict silently vanished is indistinguishable from a guard
	// that never ran.
	if err := s.recordGroomingChurn(r, runID, stageID, created.ID.String(), report); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append grooming churn audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// TERMINAL SETTLE (#2837). The report is now ingested, persisted, audited and
	// churn-checked; advance the plan stage to its gate the SAME way the plan
	// artifact does — via the SHARED advancePlanStageTerminal helper, not a
	// hand-rolled third copy — which owns both the gated (running →
	// awaiting_approval) and gateless (running → succeeded + orchestrator Advance)
	// arms plus the sticky status-comment notify. Without this the stage sits in
	// `running` forever and its approval gate never opens.
	//
	// Placement is load-bearing: it is AFTER every fail-closed guard and every 500
	// return above, so an artifact-create, audit-append or churn-append failure
	// leaves the stage in `running` for the runner's retry to heal rather than
	// advancing a not-durable report to an approvable state.
	//
	// Deliberately NOT notifyPlanReadyIfReady: notifyPlanReady resolves the run's
	// PLAN artifact through tryLoadPlanForRun, which filters a.Kind !=
	// artifact.KindPlan (prompt.go), so on a grooming run it finds no plan and
	// no-ops. advancePlanStageTerminal's own notifyStatusUpdate is the
	// operator-visible surface for this settle; a grooming-specific report-ready
	// comment is a candidate follow-up, not built here. The returned settled
	// stage is unused (no notifyPlanReadyIfReady on the grooming path), so discard
	// it rather than reassigning `stage`.
	s.advancePlanStageTerminal(r, runID, stage)

	s.writeJSON(w, r, http.StatusCreated, planResponse{
		ID:            created.ID,
		StageID:       created.StageID,
		ContentHash:   contentHash,
		SchemaVersion: schemaVersion,
		Idempotent:    false,
	})
}

// groomingEntryCounts is the per-class census recorded in the audit payload —
// the cheap run-over-run signal #2240's churn guard reads before fetching the
// report body by content hash.
func groomingEntryCounts(gr *plan.GroomingReport) map[string]int {
	counts := map[string]int{
		"ordering":                  len(gr.Ordering),
		"duplicates":                len(gr.Duplicates),
		"hygiene_defects":           len(gr.HygieneDefects),
		"dependency_edges":          len(gr.DependencyEdges),
		"vision_drift":              len(gr.VisionDrift),
		"decomposition_suggestions": len(gr.DecompositionSuggestions),
	}
	// Milestone counts (E54.9 / #2309) are emitted ONLY when the report carries
	// a milestone_scope, so an ordinary grooming report's audit payload is
	// byte-identical to before this slice.
	if gr.MilestoneScope != nil {
		counts["milestone_inclusions"] = len(gr.MilestoneScope.Included)
		counts["milestone_exclusions"] = len(gr.MilestoneScope.Excluded)
		counts["milestone_declined_calls"] = len(gr.MilestoneScope.DeclinedCalls)
	}
	return counts
}

// failGroomingStage transitions the stage to failed-B and walks the run to
// terminal, mirroring the plan-invalid and clarification-invalid paths. A
// transition failure is logged, never fatal: the 400 the caller writes is the
// runner's actionable signal either way.
func (s *Server) failGroomingStage(r *http.Request, runID, stageID uuid.UUID, reason string) {
	if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureB, reason); ferr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"grooming report upload: transition to failed-B failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", ferr.Error()))
	}
	s.advanceAfterFailure(r, runID, stageID)
}

// --- E54.8 / #2240: the churn guard, ON the real ingest path ----------------

// groomingBaselineRunScanLimit bounds how far back the baseline search walks.
//
// The cap is a runaway guard, not a tuning knob — but a cap alone is only safe
// if the window it bounds is dense in grooming runs (#2240 fix-up). Repo +
// account is NOT such a window: implement, review and deploy runs share the
// repo string, so a busy repo can push the prior grooming run past 50 rows and
// the baseline would resolve empty with nothing said about it. The query is
// therefore WORKFLOW-scoped as well (ListRunsFilter.WorkflowID, an equality
// predicate the production query already supports — empty means no
// constraint), so the window contains only runs of THIS grooming workflow and
// the prior report really is the first or second hit.
//
// The residual is named rather than assumed away: when the returned page is
// FULL and the scan still resolved no settled baseline, the verdict carries a
// baseline_scan_capped degrade so an operator can tell "first-ever run" from
// "the baseline may lie beyond the window". Either way the baseline is EMPTY,
// which fails toward proposing — the safe direction for a suppression control.
const groomingBaselineRunScanLimit = 50

// recordGroomingChurn runs the churn guard over the just-ingested report and
// writes its verdict to the audit chain as one grooming_churn_filtered entry.
//
// It is idempotent via ensureGovernanceAuditEntry keyed on (category,
// artifact_id), so a runner retry re-POSTing identical bytes heals a missing
// verdict instead of writing a second one. The guard is PURE and the artifact
// id keys the entry, so a re-run over the same report produces the same
// verdict — re-emitting would be duplication, not correction.
//
// Returns a non-nil error only when the audit write (or its existence check)
// failed. Every INPUT-resolution degrade — conventions unreadable, prior-run
// lookup failing, prior report unparseable — resolves to the conservative
// value (package-default thresholds, empty baseline) and is NAMED in the
// emitted payload's `degraded` list rather than failing the ingest: the report
// itself is valid and durable, and refusing to record a verdict computed from
// a partial baseline would hide the guard's own uncertainty.
func (s *Server) recordGroomingChurn(r *http.Request, runID, stageID uuid.UUID, artifactID string, report *plan.GroomingReport) error {
	ctx := r.Context()
	var degraded []string

	var current *run.Run
	repo := ""
	if rn, err := s.cfg.RunRepo.GetRun(ctx, runID); err == nil && rn != nil {
		current = rn
		repo = rn.Repo
	} else {
		degraded = append(degraded, "run_lookup_failed")
	}

	// AC3: the significance bar is operator-declarable, read from THIS repo's
	// work-management conventions through the process-wide loader seam. An
	// unreadable config resolves the shipped conservative defaults.
	conv := workmgmt.Default()
	if repo != "" {
		if loaded, err := conventionsLoader(ctx, repo); err == nil {
			conv = loaded
		} else {
			degraded = append(degraded, "conventions_unreadable")
		}
	}
	thresholds := conv.ResolveGroomingThresholds()

	baseline, baselineDegraded := s.groomingChurnBaseline(ctx, current)
	degraded = append(degraded, baselineDegraded...)

	result := workmgmt.FilterGroomingChurn(report, baseline, thresholds)

	payload, err := json.Marshal(map[string]any{
		"run_id":      runID.String(),
		"stage_id":    stageID.String(),
		"artifact_id": artifactID,
		"summary":     result.Summary,
		"suppressed":  result.Suppressed,
		"resurfaced":  result.Resurfaced,
		"thresholds":  thresholds,
		"degraded":    degraded,
	})
	if err != nil {
		return err
	}

	systemKind := audit.ActorKind("system")
	_, herr := s.ensureGovernanceAuditEntry(ctx, runID,
		workmgmt.GroomingChurnFilteredCategory, artifactID, func() error {
			_, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
				RunID:     runID,
				StageID:   &stageID,
				Timestamp: time.Now().UTC(),
				Category:  workmgmt.GroomingChurnFilteredCategory,
				ActorKind: &systemKind,
				Payload:   payload,
			})
			return aerr
		})
	return herr
}

// groomingChurnBaseline assembles the three-state baseline (AC4: the last
// approved AND APPLIED state, never the last proposed one) from the most
// recent PRIOR run IN THE SAME TENANT on the same repo that shipped a grooming
// report.
//
// It uses only query surfaces that already exist — ListRuns, ListStagesForRun,
// artifact ListForStage, and the audit chain — so no new DB query, sqlc
// regeneration, or repository-interface widening rides along.
//
// TENANCY (#2240 fix-up). A repo STRING is not an identity on a multi-tenant,
// multi-forge surface: ADR-057 hangs a nullable account_id off every root
// entity and installations route to different forge_base_urls (GHES and
// github.com), so two tenants can legitimately own runs whose Repo is the same
// "owner/name". Selecting a baseline on the repo string alone would let one
// tenant's dispositions decide another's suppressions AND copy that tenant's
// entry ids and counts into this run's audit payload. Scoping is therefore
// applied TWICE: the query carries AccountID, and every candidate is
// re-checked against the current run's account AND installation before it can
// become a baseline. The second check is not redundant — ListRuns' account
// predicate deliberately keeps account_id IS NULL rows visible (the untenanted
// -list contract), which is right for a listing surface and wrong for a
// suppression control.
//
// Tenant equality is EXACT, nil-installation included: a candidate whose
// installation is unset does not match one whose installation is set. An
// over-strict mismatch costs a baseline and therefore PROPOSES, which is this
// guard's fail-safe direction; an under-strict match suppresses across a
// tenant boundary, which is not recoverable by the operator.
//
// RECENCY is enforced HERE by an explicit created_at (id-tiebroken) sort
// rather than inherited from query-order convention. The production ListRuns
// does ORDER BY created_at DESC, id DESC — but a suppression control that
// silently diffs against the OLDEST baseline if that ORDER BY ever changes is
// failing in the unsafe direction against stale dispositions, and no
// repository-level test binds this function to that clause.
//
// PREDECESSOR, not merely "not me" (#2240 fix-up). Excluding the current run's
// own id is not the same boundary as "a PRIOR run", which is what this
// function claims and what AC4 means. Grooming runs on one repo can overlap:
// ListRuns returns every same-tenant run on the repo regardless of when it was
// created, so a run created AFTER this one — including a grooming run that
// started later and settled first — would sort newest-first and be adopted,
// letting FUTURE dispositions suppress findings in an EARLIER run. Candidates
// are therefore restricted to runs strictly preceding the current one in the
// SAME (created_at, id) total order the sort below declares, so "newest" is
// always read from the past half of that order. An unset current created_at
// admits no candidate and so proposes — the fail-safe direction.
//
// A newer prior run that SETTLED NOTHING must not shadow an older one that did
// (#2240 fix-up). A grooming run whose report was never decided (or whose
// applies all failed) contributes no disposition, so its baseline is
// entry-less and diffing against it proposes the whole backlog again — the
// convergence break AC1/AC4 exist to prevent, triggered by an intermediate run
// the operator never acted on. The scan therefore CONTINUES past a
// disposition-less candidate to the last SETTLED baseline, and falls back to
// the newest report's (entry-less) baseline only when no candidate settled
// anything — where it is the correct answer and carries the current charter
// revision for the charter-drift signal.
//
// Dispositions come from the prior run's grooming_mutation_applied audit rows,
// which the apply layer (#2237) writes once per SETTLED candidate. That single
// category carries both halves of the baseline: an `applied` outcome (or a
// skip whose reason is already_applied) is an APPLIED disposition, and a skip
// whose reason is not_approved or amended is a REJECTED one. Anything else —
// a failed apply, any other containment skip — records NO disposition, so the
// entry is absent from the baseline and resurfaces. That is AC4's requirement
// and the fail-safe direction in one rule.
//
// STATED GAP (#2240): nothing in production calls ApplyGrooming yet — #2237
// shipped the apply layer with its seam deliberately unwired — so today no
// grooming_mutation_applied row exists and every baseline resolves empty,
// which correctly proposes everything. The wiring here is complete and reads
// the real category, so dispositions populate the moment an apply consumer
// lands, with no change to this function.
//
// Every failure degrades to a NAMED reason and an empty-or-partial baseline
// rather than an error: a suppression control that cannot read its baseline
// must propose, not suppress.
func (s *Server) groomingChurnBaseline(ctx context.Context, current *run.Run) (workmgmt.GroomingBaseline, []string) {
	empty := workmgmt.NewGroomingBaseline(nil, nil, nil)
	if current == nil || current.Repo == "" {
		return empty, []string{"baseline_repo_unknown"}
	}
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo:      current.Repo,
		AccountID: current.AccountID,
		// Workflow-scoped so the scan window is dense in grooming runs rather
		// than diluted by the implement/review/deploy runs that share the repo
		// string — see groomingBaselineRunScanLimit. Empty is no constraint, so
		// a run with no workflow id degrades to the old repo-only window.
		WorkflowID: current.WorkflowID,
		Limit:      groomingBaselineRunScanLimit,
	})
	if err != nil {
		return empty, []string{"baseline_list_runs_failed"}
	}
	// The page came back FULL, so an older candidate may exist beyond it. Only
	// meaningful on the paths that resolve NO settled baseline, where it
	// distinguishes "first-ever grooming run" from "the baseline may be out of
	// reach"; a settled baseline found inside the window makes it irrelevant.
	capped := len(runs) >= groomingBaselineRunScanLimit

	candidates := make([]*run.Run, 0, len(runs))
	for _, rn := range runs {
		if rn == nil || !sameGroomingTenant(rn, current) || !groomingRunPrecedes(rn, current) {
			continue
		}
		candidates = append(candidates, rn)
	}
	// Newest first, id-tiebroken — the same total order the ListRuns query
	// declares, asserted here rather than assumed.
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
		}
		return candidates[i].ID.String() > candidates[j].ID.String()
	})

	// unsettled holds the NEWEST candidate that shipped a report but settled
	// nothing, used only if no older candidate settled anything either.
	var unsettled *workmgmt.GroomingBaseline
	for _, rn := range candidates {
		// A RETRIEVAL FAILURE IS NOT AN ABSENCE (#2240 fix-up). Both helpers
		// below return a found/error status rather than a bare nil, because
		// "this candidate shipped no report" and "this candidate's report
		// could not be read" must not resolve the same way: treating the
		// second as the first lets the scan walk PAST the newest candidate to
		// an OLDER settled one, and diff the current report against
		// dispositions the operator may since have superseded — a suppression
		// decided by stale state, which is the one direction this guard must
		// never fail in. So an unreadable input abandons the scan entirely and
		// returns an EMPTY baseline with a named reason: everything is
		// proposed, and the verdict says why.
		prior, perr := s.priorGroomingReport(ctx, rn.ID)
		if perr != nil {
			return empty, []string{"baseline_prior_report_unreadable"}
		}
		if prior == nil {
			continue
		}
		decisions, applied, derr := s.priorGroomingDispositions(ctx, rn.ID)
		if derr != nil {
			return empty, []string{"baseline_dispositions_unreadable"}
		}
		candidate := workmgmt.NewGroomingBaseline(prior, decisions, applied)
		if len(candidate.Entries) == 0 {
			// Undecided, or every apply failed: this run decided nothing, so it
			// is not the "last approved and applied state" AC4 diffs against.
			// Keep scanning; the last SETTLED baseline is still the truth.
			if unsettled == nil {
				unsettled = &candidate
			}
			continue
		}
		return candidate, nil
	}
	var cappedDegrade []string
	if capped {
		cappedDegrade = []string{"baseline_scan_capped"}
	}
	if unsettled != nil {
		return *unsettled, cappedDegrade
	}
	// No prior grooming run in this tenant on this repo: the first-ever run.
	// Not a degrade — an empty baseline is the CORRECT answer, and everything
	// is proposed. Unless the scan window was full, in which case the honest
	// answer is "no baseline found WITHIN the window", named as such.
	return empty, cappedDegrade
}

// groomingRunPrecedes reports whether a candidate run strictly precedes the
// current one in the (created_at, id) total order groomingChurnBaseline sorts
// by — the same order the production ListRuns query declares. Strictness is
// what excludes the current run itself, so no separate self-exclusion is
// needed; a candidate created at the same instant is ordered by id, so the
// predicate is total and deterministic rather than dependent on clock
// resolution.
func groomingRunPrecedes(candidate, current *run.Run) bool {
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.Before(current.CreatedAt)
	}
	return candidate.ID.String() < current.ID.String()
}

// sameGroomingTenant reports whether a candidate prior run belongs to the same
// tenant AND the same forge installation as the current one. Both comparands
// are exact, an unset value included: "unset" is a distinct tenancy state, not
// a wildcard that matches every account or every forge.
func sameGroomingTenant(candidate, current *run.Run) bool {
	if candidate.AccountID != current.AccountID {
		return false
	}
	switch {
	case candidate.InstallationID == nil && current.InstallationID == nil:
		return true
	case candidate.InstallationID == nil || current.InstallationID == nil:
		return false
	default:
		return *candidate.InstallationID == *current.InstallationID
	}
}

// priorGroomingReport returns the grooming report a prior run shipped.
//
// It reports THREE outcomes, not two (#2240 fix-up), because the caller's
// scan behaves differently for each and collapsing them is a suppression bug:
//
//   - (report, nil)  — found;
//   - (nil, nil)     — this run genuinely shipped no grooming report (an
//     implement-only run, say). The scan CONTINUES to older candidates,
//     which is correct: this run never had a baseline to offer;
//   - (nil, err)     — a retrieval or decode failure. The scan STOPS and the
//     caller degrades to an empty baseline. Returning "absent" here would let
//     the scan adopt an OLDER run's dispositions in place of the newest one's,
//     suppressing against state the operator may have superseded.
//
// A stage-listing or artifact-listing failure is always an error. A report
// that no longer PARSES is an error only if no sibling artifact on the stage
// parsed: a grooming_report row that fails validation next to a good one is
// junk to skip, whereas a stage whose only grooming_report is undecodable is a
// baseline that exists and cannot be read.
func (s *Server) priorGroomingReport(ctx context.Context, runID uuid.UUID) (*plan.GroomingReport, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list stages for run %s: %w", runID, err)
	}
	unparseable := 0
	for _, st := range stages {
		if st == nil || st.Type != run.StageTypePlan {
			continue
		}
		arts, aerr := s.cfg.ArtifactRepo.ListForStage(ctx, st.ID)
		if aerr != nil {
			return nil, fmt.Errorf("list artifacts for stage %s: %w", st.ID, aerr)
		}
		for _, a := range arts {
			if a == nil || a.Kind != artifact.KindGroomingReport {
				continue
			}
			report, perr := plan.ParseGroomingReport(a.Content)
			if perr != nil {
				unparseable++
				continue
			}
			return report, nil
		}
	}
	if unparseable > 0 {
		return nil, fmt.Errorf("run %s shipped %d grooming_report artifact(s), none parseable", runID, unparseable)
	}
	return nil, nil
}

// priorGroomingDispositions reads the prior run's grooming_mutation_applied
// audit rows and projects them back into the two inputs NewGroomingBaseline
// takes: the operator's rejections and the apply's landed mutations.
//
// The payload is decoded through a TOLERANT projection carrying only the three
// fields that matter (entry_id, outcome, skip_reason), so it reads a record
// marshalled bare as well as one nested under the run/stage envelope the other
// grooming categories use. An undecodable row is skipped, which contributes no
// disposition and therefore resurfaces its entry — the fail-safe direction.
//
// A failure of the QUERY ITSELF is different in kind and is returned as an
// error (#2240 fix-up): "this run recorded no dispositions" and "this run's
// dispositions could not be read" are distinct states, and returning nil for
// both let the caller treat the newest candidate as unsettled and walk on to
// an OLDER run's dispositions — suppressing against superseded state.
func (s *Server) priorGroomingDispositions(ctx context.Context, runID uuid.UUID) ([]workmgmt.GroomingDecision, *workmgmt.GroomingApplyResult, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, workmgmt.GroomingMutationAppliedCategory)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s entries for run %s: %w", workmgmt.GroomingMutationAppliedCategory, runID, err)
	}
	var decisions []workmgmt.GroomingDecision
	applied := &workmgmt.GroomingApplyResult{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		var rec struct {
			EntryID    string `json:"entry_id"`
			Outcome    string `json:"outcome"`
			SkipReason string `json:"skip_reason"`
		}
		if json.Unmarshal(e.Payload, &rec) != nil || rec.EntryID == "" {
			continue
		}
		switch {
		case rec.Outcome == string(workmgmt.GroomingOutcomeApplied):
			applied.Applied = append(applied.Applied, workmgmt.GroomingMutationRecord{
				EntryID: rec.EntryID, Outcome: workmgmt.GroomingOutcomeApplied,
			})
		case rec.SkipReason == workmgmt.GroomingSkipAlreadyApplied:
			applied.Skipped = append(applied.Skipped, workmgmt.GroomingMutationRecord{
				EntryID: rec.EntryID, Outcome: workmgmt.GroomingOutcomeSkipped,
				SkipReason: workmgmt.GroomingSkipAlreadyApplied,
			})
		case rec.SkipReason == workmgmt.GroomingSkipNotApproved:
			decisions = append(decisions, workmgmt.GroomingDecision{
				EntryID: rec.EntryID, Verdict: workmgmt.GroomingRejected,
			})
		case rec.SkipReason == workmgmt.GroomingSkipAmended:
			decisions = append(decisions, workmgmt.GroomingDecision{
				EntryID: rec.EntryID, Verdict: workmgmt.GroomingAmended,
			})
		}
	}
	return decisions, applied, nil
}
