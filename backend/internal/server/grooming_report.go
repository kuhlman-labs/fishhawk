package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

	// Validate against grooming-report-v1 plus the four semantic rules the
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
	return map[string]int{
		"ordering":                  len(gr.Ordering),
		"duplicates":                len(gr.Duplicates),
		"hygiene_defects":           len(gr.HygieneDefects),
		"dependency_edges":          len(gr.DependencyEdges),
		"vision_drift":              len(gr.VisionDrift),
		"decomposition_suggestions": len(gr.DecompositionSuggestions),
	}
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
