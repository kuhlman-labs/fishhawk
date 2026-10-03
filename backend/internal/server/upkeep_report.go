package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// CategoryUpkeepReportRecorded is the audit category appended when an
// upkeep_report artifact is ingested (#3921). Registered in
// audit.KnownCategories so fishhawk_await_audit can arm on it.
const CategoryUpkeepReportRecorded = "upkeep_report_recorded"

// upkeepRunRefInvalid is the details.reason of a 400 upkeep_report_invalid for
// a cited run that is unknown OR belongs to another repository/account. The
// two are deliberately one reason (see checkUpkeepRunRefs).
const upkeepRunRefInvalid = "run_ref_invalid"

// upkeepRefusalStageTypeNotPlan is the details.reason of a 400
// upkeep_report_stage_invalid for a report shipped from a non-plan stage.
const upkeepRefusalStageTypeNotPlan = "stage_type_not_plan"

// upkeepIngestMu serializes the upkeep-report ingest critical section
// (GetByHash, the audit-entry heal, Create and the chained append) for exactly
// the reasons groomingIngestMu documents, with the same lock ordering
// (upkeepIngestMu → governanceHealMu) and the same RESIDUAL: it is
// PROCESS-LOCAL, so two fishhawkd replicas ingesting the same report at the
// same instant can still double-write.
var upkeepIngestMu sync.Mutex

// upkeepStageAllowedKinds is the plan-path guard's ALLOWLIST: the only
// artifact kinds a stage declaring `produces: upkeep_report` may ship. Every
// kind NOT listed — plan, grooming_report, and any sibling added later — is
// refused by guardUpkeepStageProposal, so a new kind is refused until it is
// deliberately classified here. clarification_request is kept on purpose:
// parking writes nothing approvable, and an operator answer resumes the scan.
var upkeepStageAllowedKinds = map[plan.ArtifactKind]bool{
	plan.ArtifactKindUpkeepReport:         true,
	plan.ArtifactKindClarificationRequest: true,
}

// guardUpkeepStageProposal is the plan-path guard (#3921): it refuses any
// artifact kind outside upkeepStageAllowedKinds when the shipping stage's
// cached workflow declares `produces: upkeep_report` on it (or the stage cannot
// be mapped inside such a workflow). It returns true when it wrote the
// response.
//
// It fails OPEN when nothing is resolvable — a nil or unconfigured RunRepo, no
// run row, no cached spec (upkeepStageRefusesOtherProposal) — so deployments
// and runs that cannot resolve a workflow keep today's path. A store that did
// not answer is a 500 with nothing stored and the stage untouched.
//
// The refusal goes through the kind's own invalid tail: a grooming_report gets
// 400 grooming_report_stage_invalid (fail-B); every other kind — plan
// included — gets the plan path's plan_invalid tail with a *plan.SemanticError
// naming the declaration, so it is never stored.
func (s *Server) guardUpkeepStageProposal(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, kind plan.ArtifactKind) bool {
	if upkeepStageAllowedKinds[kind] {
		return false
	}
	b, err := s.resolveUpkeepStageBinding(r.Context(), runID, stage)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve the stage's upkeep_report declaration failed", map[string]any{"error": err.Error()})
		return true
	}
	if !upkeepStageRefusesOtherProposal(b) {
		return false
	}
	why := "declares produces: upkeep_report"
	if b.Undecidable != "" {
		why = "belongs to a workflow declaring produces: upkeep_report and its own declaration is undecidable (" + b.Undecidable + ")"
	}
	msg := fmt.Sprintf("stage %s %s; it may ship only an upkeep_report or a clarification_request, not a %s", stageID, why, kind)
	switch kind {
	case plan.ArtifactKindGroomingReport:
		s.failGroomingStage(r, runID, stageID, "grooming_report_stage_invalid: "+msg)
		s.writeError(w, r, http.StatusBadRequest, "grooming_report_stage_invalid", msg,
			map[string]any{"stage_type": string(stage.Type), "reason": "stage_declares_upkeep_report"})
	default:
		s.refusePlanInvalid(w, r, runID, stageID, &plan.SemanticError{Message: msg})
	}
	return true
}

// handleUpkeepReport ingests an upkeep_report artifact — the THIRD additive
// plan-stage sibling (#3921, E79) — shipped to POST /v0/runs/{run_id}/plan.
// The report is bound to the shipping stage's DECLARATION (`produces:
// upkeep_report` in the run's cached spec), never to the body's `kind` alone.
// Contract: docs/spec/upkeep-report-v1.md § "Ingest".
//
// Failure modes:
//   - non-`plan` stage                     → 400 upkeep_report_stage_invalid
//     (reason stage_type_not_plan), fail-B;
//   - stage does not declare the artifact, or the binding is undecidable
//     → 400 upkeep_report_stage_invalid (reason
//     stage_does_not_declare_upkeep_report / stage_binding_undecidable), fail-B;
//   - schema / semantic violation          → 400 upkeep_report_invalid, fail-B;
//   - a cited run unknown or foreign       → 400 upkeep_report_invalid
//     (reason run_ref_invalid, run_id), fail-B;
//   - a store that did not answer, storage → 500, stage untouched (the runner
//     retries; idempotent via GetByHash, which HEALS a missing audit row).
//
// The tracker dedupe runs BEFORE the critical section and never fails the
// ingest: a degraded dedupe is recorded (dedupe_degraded, a named reason, an
// empty duplicates array), not hidden.
func (s *Server) handleUpkeepReport(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, body []byte) {
	ctx := r.Context()
	if stage.Type != run.StageTypePlan {
		s.refuseUpkeepStage(w, r, runID, stageID, stage, upkeepRefusalStageTypeNotPlan)
		return
	}

	b, err := s.resolveUpkeepStageBinding(ctx, runID, stage)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve the stage's upkeep_report declaration failed", map[string]any{"error": err.Error()})
		return
	}
	if reason := upkeepIngestRefusal(b); reason != "" {
		s.refuseUpkeepStage(w, r, runID, stageID, stage, reason)
		return
	}

	report, err := plan.ParseUpkeepReport(body)
	if err != nil {
		s.failUpkeepStage(r, runID, stageID, "upkeep_report_invalid: "+err.Error())
		s.writeError(w, r, http.StatusBadRequest, "upkeep_report_invalid",
			"upkeep_report does not validate against upkeep-report-v1",
			map[string]any{"error": err.Error()})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if errors.Is(err, run.ErrNotFound) {
		s.refuseUpkeepStage(w, r, runID, stageID, stage, upkeepRefusalStageBindingUndecidable)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read the reporting run failed", map[string]any{"error": err.Error()})
		return
	}
	refused, ok, err := s.checkUpkeepRunRefs(ctx, runRow, report.RunRefIDs())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check cited run refs failed", map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		msg := fmt.Sprintf("cited run %s is unknown or not in this run's repository", refused)
		s.failUpkeepStage(r, runID, stageID, "upkeep_report_invalid: "+upkeepRunRefInvalid+": "+msg)
		s.writeError(w, r, http.StatusBadRequest, "upkeep_report_invalid", msg,
			map[string]any{"reason": upkeepRunRefInvalid, "run_id": refused.String()})
		return
	}

	// Outside the mutex: no forge read is held under the lock.
	dedupe := s.upkeepDuplicates(ctx, runRow, upkeepProposals(report))

	contentHash := sha256Hex(body)
	schemaVersion := plan.UpkeepReportVersion
	payload := func(artifactID string) json.RawMessage {
		m := map[string]any{
			"run_id":         runID.String(),
			"stage_id":       stageID.String(),
			"artifact_id":    artifactID,
			"content_hash":   contentHash,
			"schema_version": schemaVersion,
			"size_bytes":     len(body),
			"entry_counts":   upkeepEntryCounts(report),
			// Always a JSON array ([] when none or degraded): the adapter
			// never returns nil, and #3924 reads this key.
			"duplicates":              dedupe.Duplicates,
			"dedupe_degraded":         dedupe.Degraded,
			"dedupe_scanned_items":    dedupe.ScannedItems,
			"dedupe_window_truncated": dedupe.WindowTruncated,
		}
		if dedupe.Degraded {
			m["dedupe_degrade_reason"] = dedupe.DegradeReason
		}
		p, _ := json.Marshal(m)
		return p
	}
	systemKind := audit.ActorKind("system")
	appendEntry := func(artifactID string) error {
		_, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			StageID:   &stageID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryUpkeepReportRecorded,
			ActorKind: &systemKind,
			Payload:   payload(artifactID),
		})
		return aerr
	}

	upkeepIngestMu.Lock()
	defer upkeepIngestMu.Unlock()

	if existing, gerr := s.cfg.ArtifactRepo.GetByHash(ctx, stageID, contentHash); gerr == nil {
		// A retry of a report already durable: heal a missing recorded row
		// (Create succeeded, AppendChained failed), then settle.
		if _, herr := s.ensureGovernanceAuditEntry(ctx, runID,
			CategoryUpkeepReportRecorded, existing.ID.String(), func() error {
				return appendEntry(existing.ID.String())
			}); herr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal upkeep report audit entry failed", map[string]any{"error": herr.Error()})
			return
		}
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
			"check existing upkeep report failed", map[string]any{"error": gerr.Error()})
		return
	}

	created, err := s.cfg.ArtifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindUpkeepReport,
		SchemaVersion: &schemaVersion,
		Content:       json.RawMessage(body),
		ContentHash:   contentHash,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create upkeep report artifact failed", map[string]any{"error": err.Error()})
		return
	}
	if err := appendEntry(created.ID.String()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append upkeep report audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// TERMINAL SETTLE, after every 500 return above, so a not-durable report
	// never reaches an approvable state (the grooming path's #2837 rationale).
	s.advancePlanStageTerminal(r, runID, stage)

	s.writeJSON(w, r, http.StatusCreated, planResponse{
		ID:            created.ID,
		StageID:       created.StageID,
		ContentHash:   contentHash,
		SchemaVersion: schemaVersion,
		Idempotent:    false,
	})
}

// refuseUpkeepStage writes 400 upkeep_report_stage_invalid with reason and
// fails the stage category-B: the binding is a property of the run, so
// re-shipping the same bytes cannot help.
func (s *Server) refuseUpkeepStage(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, reason string) {
	s.failUpkeepStage(r, runID, stageID, "upkeep_report_stage_invalid: "+reason)
	s.writeError(w, r, http.StatusBadRequest, "upkeep_report_stage_invalid",
		"upkeep_report may only be shipped from a plan stage that declares produces: upkeep_report",
		map[string]any{"reason": reason, "stage_type": string(stage.Type)})
}

// upkeepEntryCounts is the per-source census recorded in the audit payload.
// Every key is always present.
func upkeepEntryCounts(r *plan.UpkeepReport) map[string]int {
	counts := map[string]int{
		"findings":                      len(r.Findings),
		plan.UpkeepSourceFlake:          0,
		plan.UpkeepSourceToolchainDrift: 0,
		plan.UpkeepSourceDeprecation:    0,
	}
	for _, f := range r.Findings {
		counts[f.Source]++
	}
	return counts
}

// upkeepProposals adapts the report's findings to the dedupe's input. It lives
// here, not in plan, so plan stays free of an upkeep import.
func upkeepProposals(r *plan.UpkeepReport) []upkeep.Proposal {
	out := make([]upkeep.Proposal, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, upkeep.Proposal{
			FindingID: f.ID,
			Title:     f.ProposedIssue.Title,
			Labels:    f.ProposedIssue.Labels,
			Type:      f.ProposedIssue.Type,
		})
	}
	return out
}

// failUpkeepStage transitions the stage to failed-B and walks the run to
// terminal, mirroring failGroomingStage. A transition failure is logged, never
// fatal: the 400 the caller writes is the runner's actionable signal.
func (s *Server) failUpkeepStage(r *http.Request, runID, stageID uuid.UUID, reason string) {
	if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureB, reason); ferr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"upkeep report upload: transition to failed-B failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", ferr.Error()))
	}
	s.advanceAfterFailure(r, runID, stageID)
}
