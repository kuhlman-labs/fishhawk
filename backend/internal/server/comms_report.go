package server

// The comms_report INGEST (E81.5 / #3775, phase 5 #4015): the plan-path guard
// a stage declaring `produces: comms_report` applies to every other kind, and
// handleCommsReport, which binds the report to the stage's declaration,
// validates it (schema + semantics, then against the stage's
// comms_scan_gathered row), previews each draft through the shared filing
// renderer (comms_filing.go) and records the artifact plus a
// comms_report_recorded row naming the exact gather. Contract:
// docs/spec/comms-report-v1.md § "Ingest" and § "Plan-path guard".

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
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// CategoryCommsReportRecorded is the audit category appended when a
// comms_report artifact is ingested (registered by #4012).
const CategoryCommsReportRecorded = audit.CommsReportRecordedCategory

// The ingest's error codes.
const (
	commsReportStageInvalidCode = "comms_report_stage_invalid"
	commsReportInvalidCode      = "comms_report_invalid"
)

// The details.reason values of a 400 comms_report_stage_invalid, and the
// guard's grooming_report refusal reason.
const (
	commsRefusalStageTypeNotPlan        = "stage_type_not_plan"
	commsRefusalStageDoesNotDeclare     = "stage_does_not_declare_comms_report"
	commsRefusalStageBindingUndecidable = "stage_binding_undecidable"
	commsRefusalScanContextAbsent       = "scan_context_absent"
	commsGuardReasonStageDeclaresComms  = "stage_declares_comms_report"
)

// The details.reason values of a 400 comms_report_invalid from the
// charter-anchored checks (checkCommsCharterRefs).
const (
	commsRefReportRefInvalid   = "report_ref_invalid"
	commsRefRubricIDUnknown    = "rubric_id_unknown"
	commsRefNonGoalIDUnknown   = "non_goal_id_unknown"
	commsRefParentEpicIsSource = "parent_epic_is_source"
)

// The 500 messages and details the ingest writes.
const (
	commsStageBindingResolveFailed = "resolve the stage's comms_report declaration failed"
	commsNoGatherValidates         = "no recorded comms_scan_gathered row for the stage validates the stored report"
)

// commsIngestMu serializes the comms-report ingest critical section
// (GetByHash, the audit-entry heal, Create and the chained append) for the
// reasons upkeepIngestMu documents, with the same lock ordering
// (commsIngestMu → governanceHealMu) and the same PROCESS-LOCAL residual. The
// gather read, the charter checks and the previews run BEFORE it, so no forge
// read is held under the lock.
var commsIngestMu sync.Mutex

// commsStageAllowedKinds is the plan-path guard's ALLOWLIST: the only artifact
// kinds a stage declaring `produces: comms_report` may ship. Every kind NOT
// listed — plan, grooming_report, upkeep_report, and any sibling added later —
// is refused by guardCommsStageProposal. clarification_request parks and
// writes nothing approvable.
var commsStageAllowedKinds = map[plan.ArtifactKind]bool{
	plan.ArtifactKindCommsReport:          true,
	plan.ArtifactKindClarificationRequest: true,
}

// guardCommsStageProposal is the comms plan-path guard: guardUpkeepStageProposal
// over the comms_report declaration. It refuses any kind outside
// commsStageAllowedKinds when the stage's cached workflow declares
// `produces: comms_report` on it (or the stage cannot be mapped inside such a
// workflow), and returns true when it wrote the response. It fails OPEN when no
// workflow resolves; a store that did not answer is a 500 with nothing stored.
// A grooming_report is refused 400 grooming_report_stage_invalid (fail-B);
// every other kind through the plan_invalid tail with a *plan.SemanticError
// naming comms_report_v1, so it is never stored.
func (s *Server) guardCommsStageProposal(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, kind plan.ArtifactKind, body []byte, derr error) bool {
	if commsStageAllowedKinds[kind] {
		return false
	}
	b, err := s.resolveStageArtifactBinding(r.Context(), runID, stage, spec.ArtifactCommsReport)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			commsStageBindingResolveFailed, map[string]any{"error": err.Error()})
		return true
	}
	if !stageRefusesOtherProposal(b) {
		return false
	}
	why := "declares produces: comms_report"
	if b.Undecidable != "" {
		why = "belongs to a workflow declaring produces: comms_report and its own declaration is undecidable (" + b.Undecidable + ")"
	}
	switch kind {
	case plan.ArtifactKindGroomingReport:
		msg := fmt.Sprintf("stage %s %s; it may ship only a comms_report or a clarification_request, not a %s", stageID, why, kind)
		s.failGroomingStage(r, runID, stageID, "grooming_report_stage_invalid: "+msg)
		s.writeError(w, r, http.StatusBadRequest, "grooming_report_stage_invalid", msg,
			map[string]any{"stage_type": string(stage.Type), "reason": commsGuardReasonStageDeclaresComms})
	default:
		msg := fmt.Sprintf("stage %s %s; %s; it may ship only a comms_report (%s, kind %q) or a clarification_request",
			stageID, why, proposalGuardBodyDetail(body, derr, plan.KindCommsReport, commsStageAllowedKinds),
			plan.CommsReportVersion, plan.KindCommsReport)
		s.refusePlanInvalid(w, r, runID, stageID, &plan.SemanticError{Message: msg})
	}
	return true
}

// commsIngestRefusal is the comms_report ingest's binding decision: the
// details.reason of a 400 comms_report_stage_invalid, or "" when the stage may
// ship a comms_report. It fails CLOSED: an undecidable binding refuses, and so
// does any binding (the zero value included) not positively declaring it.
func commsIngestRefusal(b stageArtifactBinding) string {
	if b.Undecidable != "" {
		return commsRefusalStageBindingUndecidable
	}
	if !b.StageDeclares {
		return commsRefusalStageDoesNotDeclare
	}
	return ""
}

// commsRefRefusal is one charter-anchored refusal: the details.reason, the
// message, and the extra details keys naming the offending ids.
type commsRefRefusal struct {
	Reason  string
	Message string
	Details map[string]any
}

// checkCommsCharterRefs checks report against the gather it was shown, in the
// contract's fixed order, and returns the first refusal (nil when none):
//
//  1. report_ref_invalid — a cited report id absent from gathered.Shown. The
//     shown set is authoritative: an id also listed in Omitted (a repeated id
//     renders once and its repeat is listed omitted) is citable.
//  2. rubric_id_unknown — a cited rubric id absent from the gathered charter.
//  3. non_goal_id_unknown — an n_drift non_goal_id absent from the charter.
//  4. parent_epic_is_source — a draft's parent_epic equals the issue number a
//     report it cites lives on, comment reports included.
func checkCommsCharterRefs(report *plan.CommsReport, gathered *commsScanGatheredPayload) *commsRefRefusal {
	shown := make(map[string]commsShownReport, len(gathered.Shown))
	for _, sr := range gathered.Shown {
		shown[sr.ID] = sr
	}
	for _, id := range report.CitedReportIDs() {
		if _, ok := shown[id]; !ok {
			return &commsRefRefusal{commsRefReportRefInvalid,
				fmt.Sprintf("cited report %s is not among the reports the scan was shown", id),
				map[string]any{"report_id": id}}
		}
	}
	rubric := stringSet(gathered.Charter.RubricIDs)
	for _, d := range report.Drafts {
		for _, c := range d.RubricCitations {
			if !rubric[c.RubricID] {
				return &commsRefRefusal{commsRefRubricIDUnknown,
					fmt.Sprintf("draft %s cites rubric id %s, which the gathered charter does not declare", d.ID, c.RubricID),
					map[string]any{"rubric_id": c.RubricID, "draft_id": d.ID}}
			}
		}
	}
	nonGoals := stringSet(gathered.Charter.NonGoalIDs)
	for _, n := range report.NDrift {
		if !nonGoals[n.NonGoalID] {
			return &commsRefRefusal{commsRefNonGoalIDUnknown,
				fmt.Sprintf("n_drift %s names non-goal %s, which the gathered charter does not declare", n.ID, n.NonGoalID),
				map[string]any{"non_goal_id": n.NonGoalID}}
		}
	}
	for _, d := range report.Drafts {
		if d.ProposedIssue.ParentEpic == nil {
			continue
		}
		epic, ok := plan.CommsParentEpicNumber(*d.ProposedIssue.ParentEpic)
		if !ok {
			continue // unreachable after semantic rule (i)
		}
		for _, id := range d.SourceReportIDs {
			if shown[id].IssueNumber == epic {
				return &commsRefRefusal{commsRefParentEpicIsSource,
					fmt.Sprintf("draft %s names parent_epic %s, the issue its cited report %s lives on", d.ID, *d.ProposedIssue.ParentEpic, id),
					map[string]any{"draft_id": d.ID, "parent_epic": *d.ProposedIssue.ParentEpic}}
			}
		}
	}
	return nil
}

// stringSet returns xs as a set.
func stringSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// commsUnaccountedReportIDs returns the gather's shown ids the report cites
// nowhere, sorted and distinct; always non-nil.
func commsUnaccountedReportIDs(report *plan.CommsReport, gathered *commsScanGatheredPayload) []string {
	cited := stringSet(report.CitedReportIDs())
	seen := map[string]bool{}
	out := []string{}
	for _, sr := range gathered.Shown {
		if cited[sr.ID] || seen[sr.ID] {
			continue
		}
		seen[sr.ID] = true
		out = append(out, sr.ID)
	}
	sort.Strings(out)
	return out
}

// commsBoundGather is the gather a report is bound to: its audit row and the
// strictly decoded payload.
type commsBoundGather struct {
	entry   *audit.Entry
	payload *commsScanGatheredPayload
}

// commsRecordedPayload is the comms_report_recorded row (contract
// § "comms_report_recorded").
func commsRecordedPayload(runID, stageID uuid.UUID, artifactID, contentHash string, sizeBytes int, report *plan.CommsReport, g commsBoundGather, previews commsPreviewSet) json.RawMessage {
	m := map[string]any{
		"run_id":         runID.String(),
		"stage_id":       stageID.String(),
		"artifact_id":    artifactID,
		"content_hash":   contentHash,
		"schema_version": plan.CommsReportVersion,
		"size_bytes":     sizeBytes,
		"entry_counts": map[string]int{
			"drafts": len(report.Drafts), "n_drift": len(report.NDrift), "not_drafted": len(report.NotDrafted),
		},
		"gather_digest":          g.payload.GatherDigest,
		"gather_sequence":        g.entry.Sequence,
		"charter_content_hash":   g.payload.Charter.ContentHash,
		"unaccounted_report_ids": commsUnaccountedReportIDs(report, g.payload),
		"previews":               previews.Previews,
		"preview_degraded":       previews.Degraded,
		"charter_text":           previews.CharterText,
	}
	if previews.Degraded {
		m["preview_degrade_reason"] = previews.DegradeReason
	}
	p, _ := json.Marshal(m)
	return p
}

// handleCommsReport ingests a comms_report artifact — the FOURTH additive
// plan-stage sibling (#4015) — shipped to POST /v0/runs/{run_id}/plan.
//
// Order (contract § "Ingest"): (1) stage binding; (2) schema + semantics;
// (3) the existing-artifact check — a stored comms_report with the same
// content hash takes the idempotent path BEFORE any gather is read, so a later
// gather can never fail an already-committed stage; (4) charter-anchored
// validation against the stage's LATEST comms_scan_gathered row; (5) previews,
// outside the lock; (6) artifact + comms_report_recorded (naming that gather's
// digest and sequence) + settle.
//
// Failure modes: every 400 (comms_report_stage_invalid reasons
// stage_type_not_plan / stage_does_not_declare_comms_report /
// stage_binding_undecidable / scan_context_absent; comms_report_invalid with
// no reason for schema/semantic failures and reasons report_ref_invalid /
// rubric_id_unknown / non_goal_id_unknown / parent_epic_is_source) fails the
// stage category-B; a store that did not answer is a 500 with the stage left
// running. A preview failure is recorded, never fatal. The ingest never touches
// cfg.UserReportCursors.
func (s *Server) handleCommsReport(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, body []byte) {
	ctx := r.Context()
	if stage.Type != run.StageTypePlan {
		s.refuseCommsStage(w, r, runID, stageID, stage, commsRefusalStageTypeNotPlan)
		return
	}
	b, err := s.resolveStageArtifactBinding(ctx, runID, stage, spec.ArtifactCommsReport)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			commsStageBindingResolveFailed, map[string]any{"error": err.Error()})
		return
	}
	if reason := commsIngestRefusal(b); reason != "" {
		s.refuseCommsStage(w, r, runID, stageID, stage, reason)
		return
	}

	report, err := plan.ParseCommsReport(body)
	if err != nil {
		s.failCommsStage(r, runID, stageID, commsReportInvalidCode+": "+err.Error())
		s.writeError(w, r, http.StatusBadRequest, commsReportInvalidCode,
			"comms_report does not validate against comms-report-v1",
			map[string]any{"error": err.Error()})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if errors.Is(err, run.ErrNotFound) {
		s.refuseCommsStage(w, r, runID, stageID, stage, commsRefusalStageBindingUndecidable)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read the reporting run failed", map[string]any{"error": err.Error()})
		return
	}

	contentHash := sha256Hex(body)

	// (3) The existing-artifact check, BEFORE any gather is read.
	existing, gerr := s.cfg.ArtifactRepo.GetByHash(ctx, stageID, contentHash)
	switch {
	case gerr == nil:
		s.commsIdempotent(w, r, runID, stageID, stage, runRow, report, existing, len(body))
		return
	case !errors.Is(gerr, artifact.ErrNotFound):
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing comms report failed", map[string]any{"error": gerr.Error()})
		return
	}

	// (4) Charter-anchored validation against the stage's latest gather.
	gEntry, gathered, err := s.latestCommsScanGathered(ctx, runID, stageID)
	if errors.Is(err, errCommsScanUnbound) {
		s.refuseCommsStage(w, r, runID, stageID, stage, commsRefusalScanContextAbsent)
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read the stage's comms scan gather failed", map[string]any{"error": err.Error()})
		return
	}
	if ref := checkCommsCharterRefs(report, gathered); ref != nil {
		s.failCommsStage(r, runID, stageID, commsReportInvalidCode+": "+ref.Reason+": "+ref.Message)
		details := map[string]any{"reason": ref.Reason}
		for k, v := range ref.Details {
			details[k] = v
		}
		s.writeError(w, r, http.StatusBadRequest, commsReportInvalidCode, ref.Message, details)
		return
	}
	bound := commsBoundGather{entry: gEntry, payload: gathered}

	// (5) Previews, outside the lock.
	previews := s.commsPreviewDrafts(ctx, runRow, report, gathered)

	commsIngestMu.Lock()
	defer commsIngestMu.Unlock()

	// A concurrent identical POST may have committed while this one previewed.
	if existing, gerr := s.cfg.ArtifactRepo.GetByHash(ctx, stageID, contentHash); gerr == nil {
		s.commsSettleExisting(w, r, runID, stage, existing, func() json.RawMessage {
			return commsRecordedPayload(runID, stageID, existing.ID.String(), contentHash, len(body), report, bound, previews)
		})
		return
	} else if !errors.Is(gerr, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing comms report failed", map[string]any{"error": gerr.Error()})
		return
	}

	schemaVersion := plan.CommsReportVersion
	created, err := s.cfg.ArtifactRepo.Create(ctx, artifact.CreateParams{
		StageID:       stageID,
		Kind:          artifact.KindCommsReport,
		SchemaVersion: &schemaVersion,
		Content:       json.RawMessage(body),
		ContentHash:   contentHash,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create comms report artifact failed", map[string]any{"error": err.Error()})
		return
	}
	if err := s.appendCommsRecorded(ctx, runID, stageID,
		commsRecordedPayload(runID, stageID, created.ID.String(), contentHash, len(body), report, bound, previews)); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append comms report audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// TERMINAL SETTLE, after every 500 return above (the upkeep rationale).
	s.advancePlanStageTerminal(r, runID, stage)
	s.writeJSON(w, r, http.StatusCreated, planResponse{
		ID:            created.ID,
		StageID:       created.StageID,
		ContentHash:   contentHash,
		SchemaVersion: schemaVersion,
		Idempotent:    false,
	})
}

// commsIdempotent is the retry of a report already durable for this stage. It
// NEVER validates against the latest gather. When the comms_report_recorded
// row exists it only settles; when it is missing (Create succeeded, the
// append failed) the heal binds to the NEWEST of the stage's gathers the
// stored report validates against — the one the first ingest validated
// against unless a later gather it also passes exists — and re-renders the
// previews at retry time.
func (s *Server) commsIdempotent(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, runRow *run.Run, report *plan.CommsReport, existing *artifact.Artifact, sizeBytes int) {
	ctx := r.Context()
	row, err := s.recordedReportRow(ctx, runID, CategoryCommsReportRecorded, existing.ID.String())
	if err != nil && errors.As(err, new(*reportRowsListError)) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"heal comms report audit entry failed", map[string]any{"error": err.Error()})
		return
	}
	var heal func() json.RawMessage
	if row == nil {
		bound, ok, gerr := s.commsGatherValidating(ctx, runID, stageID, report)
		if gerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal comms report audit entry failed", map[string]any{"error": gerr.Error()})
			return
		}
		if !ok {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				commsNoGatherValidates, map[string]any{"artifact_id": existing.ID.String()})
			return
		}
		previews := s.commsPreviewDrafts(ctx, runRow, report, bound.payload)
		heal = func() json.RawMessage {
			return commsRecordedPayload(runID, stageID, existing.ID.String(), existing.ContentHash, sizeBytes, report, bound, previews)
		}
	}

	commsIngestMu.Lock()
	defer commsIngestMu.Unlock()
	s.commsSettleExisting(w, r, runID, stage, existing, heal)
}

// commsSettleExisting heals a missing comms_report_recorded row for existing
// (payload is called only then; a nil payload with the row missing is a 500),
// settles the stage and writes 200 idempotent. The caller holds commsIngestMu.
func (s *Server) commsSettleExisting(w http.ResponseWriter, r *http.Request, runID uuid.UUID, stage *run.Stage, existing *artifact.Artifact, payload func() json.RawMessage) {
	ctx := r.Context()
	if _, herr := s.ensureGovernanceAuditEntry(ctx, runID, CategoryCommsReportRecorded, existing.ID.String(), func() error {
		if payload == nil {
			return errors.New("the comms_report_recorded row appeared and vanished during the retry")
		}
		return s.appendCommsRecorded(ctx, runID, stage.ID, payload())
	}); herr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"heal comms report audit entry failed", map[string]any{"error": herr.Error()})
		return
	}
	s.advancePlanStageTerminal(r, runID, stage)
	s.writeJSON(w, r, http.StatusOK, planResponse{
		ID:            existing.ID,
		StageID:       existing.StageID,
		ContentHash:   existing.ContentHash,
		SchemaVersion: plan.CommsReportVersion,
		Idempotent:    true,
	})
}

// commsGatherValidating returns the stage's highest-sequence comms_scan_gathered
// row that strictly decodes, names the stage, and against which report passes
// checkCommsCharterRefs. ok is false when none does; err only for a store that
// did not answer.
func (s *Server) commsGatherValidating(ctx context.Context, runID, stageID uuid.UUID, report *plan.CommsReport) (commsBoundGather, bool, error) {
	if s.cfg.AuditRepo == nil {
		return commsBoundGather{}, false, errors.New("comms gather: audit repository not configured")
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCommsScanGathered)
	if err != nil {
		return commsBoundGather{}, false, fmt.Errorf("comms gather: list rows: %w", err)
	}
	var stageRows []*audit.Entry
	for _, e := range entries {
		if e != nil && e.StageID != nil && *e.StageID == stageID {
			stageRows = append(stageRows, e)
		}
	}
	// Highest sequence first; a tie puts the later-listed row first, as
	// latestCommsScanGathered does (reverse, then a stable sort).
	for i, j := 0, len(stageRows)-1; i < j; i, j = i+1, j-1 {
		stageRows[i], stageRows[j] = stageRows[j], stageRows[i]
	}
	sort.SliceStable(stageRows, func(i, j int) bool { return stageRows[i].Sequence > stageRows[j].Sequence })
	for _, e := range stageRows {
		p, derr := decodeCommsScanGathered(e.Payload)
		if derr != nil || p.StageID != stageID {
			continue
		}
		if checkCommsCharterRefs(report, p) == nil {
			return commsBoundGather{entry: e, payload: p}, true, nil
		}
	}
	return commsBoundGather{}, false, nil
}

// appendCommsRecorded appends one comms_report_recorded row.
func (s *Server) appendCommsRecorded(ctx context.Context, runID, stageID uuid.UUID, payload json.RawMessage) error {
	systemKind := audit.ActorKind("system")
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryCommsReportRecorded,
		ActorKind: &systemKind,
		Payload:   payload,
	})
	return err
}

// refuseCommsStage writes 400 comms_report_stage_invalid with reason and fails
// the stage category-B: the binding and the gather are properties of the run,
// so re-shipping the same bytes cannot help.
func (s *Server) refuseCommsStage(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, stage *run.Stage, reason string) {
	s.failCommsStage(r, runID, stageID, commsReportStageInvalidCode+": "+reason)
	msg := "comms_report may only be shipped from a plan stage that declares produces: comms_report"
	if reason == commsRefusalScanContextAbsent {
		msg = "no comms_scan_gathered row is recorded for this stage, so the comms_report cannot be checked against what the scan was shown"
	}
	s.writeError(w, r, http.StatusBadRequest, commsReportStageInvalidCode, msg,
		map[string]any{"reason": reason, "stage_type": string(stage.Type)})
}

// failCommsStage transitions the stage to failed-B and walks the run to
// terminal, mirroring failUpkeepStage. A transition failure is logged, never
// fatal: the 400 the caller writes is the runner's actionable signal.
func (s *Server) failCommsStage(r *http.Request, runID, stageID uuid.UUID, reason string) {
	if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureB, reason); ferr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"comms report upload: transition to failed-B failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", ferr.Error()))
	}
	s.advanceAfterFailure(r, runID, stageID)
}
