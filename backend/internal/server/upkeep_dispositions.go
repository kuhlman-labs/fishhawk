package server

// Per-finding upkeep disposition CAPTURE (#3923, E79 / #3726).
//
// The captain records a verdict — approved / rejected — against an INDIVIDUAL
// upkeep-report finding, keyed by its derived finding id, optionally with a
// parent_epic override and a BOOLEAN authorize_delegation_tier (the captain's
// authorization to apply the finding's own proposed autonomy:* label; grooming
// never applies tier labels, so this per-finding opt-in is new). Each accepted
// entry is ONE upkeep_disposition_recorded row; the #3924 apply consumes them.
//
// It reuses the grooming capture's shape (#2843 / #2991): the same
// operator-only ladder (requireOperatorCapture), the same single-document
// decode, batch-atomic validation, last-wins read-back, and the audit-layer
// capture/apply WINDOW, generalized to the upkeep family
// (audit.UpkeepWindowAppender, watermark upkeep_apply_window_closed).
//
// THE BINDING RULE departs from grooming's newest-artifact rule on purpose:
// dispositions bind to the artifact named by the HIGHEST-sequence
// upkeep_report_recorded row on the run's chain (latestUpkeepReport). That row
// is what settles the stage to awaiting_approval and carries the dedupe verdict
// #3924 consumes, so it names the report the gate is deciding. The atomic
// append RE-CHECKS that binding under the run-row lock and refuses 409
// upkeep_report_superseded when a newer report was recorded after resolution.
// Contract: docs/spec/upkeep-report-v1.md § "Dispositions".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryUpkeepDispositionRecorded aliases the audit-layer constant so the
// capture handler and the window protocol's consumed-set scan share one value.
const CategoryUpkeepDispositionRecorded = audit.UpkeepDispositionRecordedCategory

// upkeepMaxDispositions bounds one capture batch: the report's findings
// maxItems (docs/spec/upkeep-report-v1.schema.json), so a batch can name every
// finding once and no more.
const upkeepMaxDispositions = 200

// Upkeep verdicts: the CLOSED set a disposition must name.
const (
	upkeepVerdictApproved = "approved"
	upkeepVerdictRejected = "rejected"
)

var upkeepVerdictNames = []string{upkeepVerdictApproved, upkeepVerdictRejected}

// upkeepOperatorOnly is the upkeep capture's operator-only ladder text.
var upkeepOperatorOnly = operatorOnlyMessages{
	runToken:      "a run-bound agent token may not record an upkeep disposition; deciding an upkeep finding is a captain action",
	operatorAgent: "a delegated operator-agent token may not record an upkeep disposition; the upkeep report is agent-authored, so an agent recording the verdict would convert the captain gate into a self-approval",
}

// upkeepDispositionInput is one requested disposition. ParentEpic is a
// pointer so a present-but-empty value is validated (and refused) rather than
// read as absent.
type upkeepDispositionInput struct {
	FindingID               string  `json:"finding_id"`
	Verdict                 string  `json:"verdict"`
	AuthorizeDelegationTier bool    `json:"authorize_delegation_tier,omitempty"`
	ParentEpic              *string `json:"parent_epic,omitempty"`
}

type upkeepDispositionRequest struct {
	Dispositions []upkeepDispositionInput `json:"dispositions"`
}

// upkeepDispositionPayload is the audit-row payload, marshalled BARE so the
// audit layer reads artifact_id straight off it. authorize_delegation_tier is
// ALWAYS present; parent_epic only when the captain supplied one.
type upkeepDispositionPayload struct {
	RunID                   string `json:"run_id"`
	StageID                 string `json:"stage_id"`
	ArtifactID              string `json:"artifact_id"`
	ContentHash             string `json:"content_hash"`
	FindingID               string `json:"finding_id"`
	Source                  string `json:"source"`
	Verdict                 string `json:"verdict"`
	AuthorizeDelegationTier bool   `json:"authorize_delegation_tier"`
	ParentEpic              string `json:"parent_epic,omitempty"`
}

// recordedUpkeepDisposition is one projected read-back row.
type recordedUpkeepDisposition struct {
	FindingID               string `json:"finding_id"`
	Source                  string `json:"source"`
	Verdict                 string `json:"verdict"`
	AuthorizeDelegationTier bool   `json:"authorize_delegation_tier"`
	ParentEpic              string `json:"parent_epic,omitempty"`
	RecordedAt              string `json:"recorded_at"`
	RecordedBy              string `json:"recorded_by"`
	AuditSequence           int64  `json:"audit_sequence"`
}

// upkeepDispositionsResponse is the body BOTH verbs return.
type upkeepDispositionsResponse struct {
	RunID        string                      `json:"run_id"`
	ArtifactID   string                      `json:"artifact_id"`
	StageID      string                      `json:"stage_id"`
	ContentHash  string                      `json:"content_hash"`
	WindowClosed bool                        `json:"window_closed"`
	Settlement   *groomingWindowSettlement   `json:"settlement,omitempty"`
	Dispositions []recordedUpkeepDisposition `json:"dispositions"`
}

// errUpkeepReportAbsent: the run's chain carries no upkeep_report_recorded
// row — distinct from a read/parse FAILURE, which is a wrapped error.
var errUpkeepReportAbsent = errors.New("run carries no recorded upkeep_report")

// upkeepReportBinding is the resolved report a capture attaches to.
type upkeepReportBinding struct {
	art     *artifact.Artifact
	report  *plan.UpkeepReport
	stageID uuid.UUID
}

// latestUpkeepReport resolves the report dispositions bind to: the artifact
// named by the HIGHEST-sequence upkeep_report_recorded row on the run's chain.
// The #3924 apply MUST resolve through this same function.
//
// Outcomes stay distinct: no recorded row → errUpkeepReportAbsent (so an
// orphan artifact whose recorded row never landed is NOT bindable until an
// idempotent retry heals its row); an undecodable newest row, an unreadable
// artifact, a wrong kind or a parse failure → a wrapped error (500), never a
// silent fallback to an older report.
func (s *Server) latestUpkeepReport(ctx context.Context, runID uuid.UUID) (*upkeepReportBinding, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, audit.UpkeepReportRecordedCategory)
	if err != nil {
		return nil, fmt.Errorf("list upkeep_report_recorded rows for run %s: %w", runID, err)
	}
	var newest *audit.Entry
	for _, e := range rows {
		if e == nil {
			continue
		}
		// >= so a tie (impossible on one chain) takes the later-listed row.
		if newest == nil || e.Sequence >= newest.Sequence {
			newest = e
		}
	}
	if newest == nil {
		return nil, errUpkeepReportAbsent
	}
	var p struct {
		ArtifactID string `json:"artifact_id"`
	}
	if jerr := json.Unmarshal(newest.Payload, &p); jerr != nil {
		return nil, fmt.Errorf("decode upkeep_report_recorded row %d: %w", newest.Sequence, jerr)
	}
	artID, perr := uuid.Parse(p.ArtifactID)
	if perr != nil {
		return nil, fmt.Errorf("upkeep_report_recorded row %d names artifact %q: %w", newest.Sequence, p.ArtifactID, perr)
	}
	art, gerr := s.cfg.ArtifactRepo.Get(ctx, artID)
	if gerr != nil {
		return nil, fmt.Errorf("read upkeep_report artifact %s: %w", artID, gerr)
	}
	if art.Kind != artifact.KindUpkeepReport {
		return nil, fmt.Errorf("artifact %s named by upkeep_report_recorded row %d has kind %q, want %q", artID, newest.Sequence, art.Kind, artifact.KindUpkeepReport)
	}
	report, rerr := plan.ParseUpkeepReport(art.Content)
	if rerr != nil {
		return nil, fmt.Errorf("parse upkeep_report artifact %s: %w", artID, rerr)
	}
	return &upkeepReportBinding{art: art, report: report, stageID: art.StageID}, nil
}

// writeUpkeepReportResolveError maps latestUpkeepReport's outcomes.
func (s *Server) writeUpkeepReportResolveError(w http.ResponseWriter, r *http.Request, runID uuid.UUID, rerr error) {
	if errors.Is(rerr, errUpkeepReportAbsent) {
		s.writeError(w, r, http.StatusConflict, "upkeep_report_absent",
			"this run has no recorded upkeep_report; there is nothing to disposition",
			map[string]any{"run_id": runID.String()})
		return
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
		"upkeep-dispositions: resolving the recorded upkeep_report failed",
		slog.String("run_id", runID.String()), slog.String("error", rerr.Error()))
	s.writeError(w, r, http.StatusInternalServerError, "internal_error",
		"the run's recorded upkeep_report could not be read or parsed",
		map[string]any{"error": rerr.Error()})
}

// upkeepRequireRun is the 404 rung: requireRunAccount falls through on an
// unknown run, and an empty chain would otherwise read as upkeep_report_absent.
func (s *Server) upkeepRequireRun(w http.ResponseWriter, r *http.Request, runID uuid.UUID) bool {
	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"run not found", map[string]any{"run_id": runID.String()})
			return false
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read run failed", map[string]any{"error": err.Error()})
		return false
	}
	return true
}

// upkeepParseRunID is the run_id path rung shared by both verbs.
func (s *Server) upkeepParseRunID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return uuid.Nil, false
	}
	return runID, true
}

// upkeepUnconfigured is the U0 rung shared by both verbs.
func (s *Server) upkeepUnconfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "upkeep_dispositions_unconfigured",
			"upkeep-dispositions endpoint requires run + artifact + audit repositories", nil)
		return true
	}
	return false
}

// handleRecordUpkeepDispositions implements
// POST /v0/runs/{run_id}/upkeep-dispositions.
//
// EVERY rung runs BEFORE any write, and the WHOLE batch is validated before
// ANY row is appended:
//
//	U0  503 upkeep_dispositions_unconfigured
//	U1  401 authentication_required
//	U2  403 run_token_forbidden
//	U3  403 operator_agent_forbidden
//	U4  403 insufficient_scope (write:approvals, unconditional)
//	U5  400 validation_failed — bad run_id
//	U6  404 run_not_found
//	U7  400 validation_failed — unparseable body, trailing content, empty
//	        batch, > 200 entries, empty or duplicate finding_id, invalid
//	        parent_epic, parent_epic or authorize_delegation_tier:true on a
//	        rejected verdict
//	U8  400 upkeep_verdict_invalid — verdict outside {approved, rejected}
//	U9  409 upkeep_report_absent / 500 unreadable report
//	U10 422 upkeep_finding_unknown — whole-batch check
//	U11 409 upkeep_window_closed; 409 upkeep_report_superseded (atomic path)
func (s *Server) handleRecordUpkeepDispositions(w http.ResponseWriter, r *http.Request) {
	if s.upkeepUnconfigured(w, r) { // U0
		return
	}
	id, ok := s.requireOperatorCapture(w, r, upkeepOperatorOnly) // U1..U4
	if !ok {
		return
	}
	runID, ok := s.upkeepParseRunID(w, r) // U5
	if !ok {
		return
	}
	if !s.upkeepRequireRun(w, r, runID) { // U6
		return
	}

	// U7 / U8: body shape and per-entry validation.
	var reqBody upkeepDispositionRequest
	if !s.decodeSingleJSONBody(w, r, &reqBody,
		"request body must be valid JSON {dispositions:[{finding_id, verdict, authorize_delegation_tier, parent_epic}]}") {
		return
	}
	if len(reqBody.Dispositions) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"dispositions must name at least one finding; an empty capture records nothing and is ambiguous intent",
			map[string]any{"field": "dispositions"})
		return
	}
	if n := len(reqBody.Dispositions); n > upkeepMaxDispositions {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("dispositions names %d entries; at most %d (the report's findings maxItems) are allowed", n, upkeepMaxDispositions),
			map[string]any{"field": "dispositions", "max": upkeepMaxDispositions, "got": n})
		return
	}
	seen := make(map[string]struct{}, len(reqBody.Dispositions))
	for i := range reqBody.Dispositions {
		d := &reqBody.Dispositions[i]
		field := func(name string) string { return fmt.Sprintf("dispositions[%d].%s", i, name) }
		d.FindingID = strings.TrimSpace(d.FindingID)
		if d.FindingID == "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"finding_id is required on every disposition", map[string]any{"field": field("finding_id")})
			return
		}
		if _, dup := seen[d.FindingID]; dup {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"finding_id appears more than once in this batch; one request carrying two verdicts for one finding is ambiguous — record the correcting verdict as a separate request, which supersedes the earlier one",
				map[string]any{"field": field("finding_id"), "finding_id": d.FindingID})
			return
		}
		seen[d.FindingID] = struct{}{}

		d.Verdict = strings.TrimSpace(d.Verdict)
		if d.Verdict != upkeepVerdictApproved && d.Verdict != upkeepVerdictRejected { // U8
			s.writeError(w, r, http.StatusBadRequest, "upkeep_verdict_invalid",
				"verdict must name one of the upkeep verdicts",
				map[string]any{"field": field("verdict"), "got": d.Verdict, "allowed": upkeepVerdictNames})
			return
		}
		if d.ParentEpic != nil && !plan.UpkeepValidEpicRef(*d.ParentEpic) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"parent_epic must be a positive issue number, bare (389) or #-prefixed (#389)",
				map[string]any{"field": field("parent_epic"), "got": *d.ParentEpic})
			return
		}
		if d.Verdict == upkeepVerdictRejected {
			// A rejected finding files nothing, so an override on it is
			// contradictory intent, not a no-op to record.
			if d.ParentEpic != nil {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"parent_epic is an override for the issue an APPROVED finding files; a rejected finding files nothing",
					map[string]any{"field": field("parent_epic")})
				return
			}
			if d.AuthorizeDelegationTier {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"authorize_delegation_tier authorizes the proposed autonomy label of an APPROVED finding's issue; a rejected finding files nothing",
					map[string]any{"field": field("authorize_delegation_tier")})
				return
			}
		}
	}

	// U9: resolve the binding. Absent and unreadable stay DISTINCT.
	b, rerr := s.latestUpkeepReport(r.Context(), runID)
	if rerr != nil {
		s.writeUpkeepReportResolveError(w, r, runID, rerr)
		return
	}

	// U10: every finding id must be one the report declares — checked for the
	// WHOLE batch before any append, which is what makes capture batch-atomic.
	sources := make(map[string]string, len(b.report.Findings))
	for _, f := range b.report.Findings {
		sources[f.ID] = f.Source
	}
	var unknown []string
	for _, d := range reqBody.Dispositions {
		if _, ok := sources[d.FindingID]; !ok {
			unknown = append(unknown, d.FindingID)
		}
	}
	if len(unknown) > 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, "upkeep_finding_unknown",
			"one or more finding_id values are not declared by this run's recorded upkeep_report; NO disposition was recorded",
			map[string]any{"unknown_finding_ids": unknown, "artifact_id": b.art.ID.String()})
		return
	}

	// Marshal EVERY payload before any append.
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser
	stageID := b.stageID
	now := time.Now().UTC()
	params := make([]audit.ChainAppendParams, 0, len(reqBody.Dispositions))
	for _, d := range reqBody.Dispositions {
		pl := upkeepDispositionPayload{
			RunID: runID.String(), StageID: stageID.String(),
			ArtifactID: b.art.ID.String(), ContentHash: b.art.ContentHash,
			FindingID: d.FindingID, Source: sources[d.FindingID],
			Verdict: d.Verdict, AuthorizeDelegationTier: d.AuthorizeDelegationTier,
		}
		if d.ParentEpic != nil {
			pl.ParentEpic = *d.ParentEpic
		}
		payload, merr := json.Marshal(pl)
		if merr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"marshal disposition payload failed", map[string]any{"error": merr.Error()})
			return
		}
		params = append(params, audit.ChainAppendParams{
			RunID: runID, StageID: &stageID, Timestamp: now,
			Category:  CategoryUpkeepDispositionRecorded,
			ActorKind: &actorKind, ActorSubject: &subject, Payload: payload,
		})
	}

	if appender, ok := s.cfg.AuditRepo.(audit.UpkeepWindowAppender); ok {
		// ATOMIC BATCH: one capture is one transaction under the run-row lock,
		// which re-checks the binding and the window before appending.
		_, aerr := appender.AppendChainedUpkeepDispositionBatch(r.Context(), b.art.ID.String(), params)
		var (
			closed     *audit.UpkeepWindowClosedError
			superseded *audit.UpkeepReportSupersededError
		)
		switch {
		case errors.As(aerr, &superseded):
			s.writeError(w, r, http.StatusConflict, "upkeep_report_superseded",
				"a newer upkeep_report was recorded on this run after this capture resolved its report; NO disposition was recorded — re-capture against the current report",
				map[string]any{
					"artifact_id":         superseded.ArtifactID,
					"current_artifact_id": superseded.CurrentArtifactID,
					"current_sequence":    superseded.CurrentSequence,
				})
			return
		case errors.As(aerr, &closed):
			s.writeUpkeepWindowClosed(w, r, closed.ArtifactID, closed.Settlement, closed.Sequence)
			return
		case aerr != nil:
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"upkeep-dispositions: atomic disposition batch failed",
				slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"recording the disposition batch failed; the capture is atomic, so NOTHING was recorded and a repeat POST is safe",
				map[string]any{"error": aerr.Error()})
			return
		}
	} else {
		// FALLBACK (in-memory repos without the capability): check the window,
		// then a per-row loop. NON-ATOMIC — no in-transaction binding re-check,
		// and a mid-batch failure can leave durable partial rows; `recorded` /
		// `requested` are the operator's evidence of what survived. Production
		// always takes the atomic path (postgres.go assertion + decisionindex
		// forward).
		settlement, serr := s.windowSettlementFor(r.Context(), runID, audit.UpkeepApplyWindowClosedCategory, b.art.ID.String())
		if serr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"listing the upkeep capture window failed", map[string]any{"error": serr.Error()})
			return
		}
		if settlement != nil {
			s.writeUpkeepWindowClosed(w, r, b.art.ID.String(), settlement.Settlement, settlement.AuditSequence)
			return
		}
		for n := range params {
			if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), params[n]); aerr != nil {
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"recording the disposition batch failed part-way; the rows already appended are durable and a repeat POST is safe (capture is last-wins)",
					map[string]any{"recorded": n, "requested": len(params), "error": aerr.Error()})
				return
			}
		}
	}

	s.respondUpkeepDispositions(w, r, runID, b)
}

// writeUpkeepWindowClosed is the U11 refusal, shared by both append paths.
func (s *Server) writeUpkeepWindowClosed(w http.ResponseWriter, r *http.Request, artifactID, settlement string, seq int64) {
	s.writeError(w, r, http.StatusConflict, "upkeep_window_closed",
		"this upkeep report's disposition-capture window has been settled by the apply; NO disposition was recorded and the dispositions you sent are not consumable",
		map[string]any{"artifact_id": artifactID, "settlement": settlement, "watermark_sequence": seq})
}

// handleListUpkeepDispositions implements
// GET /v0/runs/{run_id}/upkeep-dispositions: read access only (the report is
// already agent-readable; the operator-only posture is scoped to capture).
func (s *Server) handleListUpkeepDispositions(w http.ResponseWriter, r *http.Request) {
	if s.upkeepUnconfigured(w, r) {
		return
	}
	runID, ok := s.upkeepParseRunID(w, r)
	if !ok {
		return
	}
	if !s.upkeepRequireRun(w, r, runID) {
		return
	}
	b, rerr := s.latestUpkeepReport(r.Context(), runID)
	if rerr != nil {
		s.writeUpkeepReportResolveError(w, r, runID, rerr)
		return
	}
	s.respondUpkeepDispositions(w, r, runID, b)
}

// respondUpkeepDispositions writes the 200 shared by both verbs, so the POST
// echo and the GET read-back are the same bytes by construction.
func (s *Server) respondUpkeepDispositions(w http.ResponseWriter, r *http.Request, runID uuid.UUID, b *upkeepReportBinding) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryUpkeepDispositionRecorded)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"listing recorded dispositions failed", map[string]any{"error": err.Error()})
		return
	}
	settlement, serr := s.windowSettlementFor(r.Context(), runID, audit.UpkeepApplyWindowClosedCategory, b.art.ID.String())
	if serr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"listing the upkeep capture window failed", map[string]any{"error": serr.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, upkeepDispositionsResponse{
		RunID:        runID.String(),
		ArtifactID:   b.art.ID.String(),
		StageID:      b.stageID.String(),
		ContentHash:  b.art.ContentHash,
		WindowClosed: settlement != nil,
		Settlement:   settlement,
		Dispositions: projectUpkeepDispositions(entries, b.art.ID.String()),
	})
}

// projectUpkeepDispositions collapses the run's upkeep_disposition_recorded
// rows into the current set for ONE artifact: undecodable rows are SKIPPED
// (a junk row must not manufacture a verdict), rows of another artifact are
// excluded, repeats collapse LAST-WINS by audit sequence (both rows stay in the
// chain), and output is sorted by finding_id.
func projectUpkeepDispositions(entries []*audit.Entry, artifactID string) []recordedUpkeepDisposition {
	byID := make(map[string]recordedUpkeepDisposition)
	for _, e := range entries {
		if e == nil {
			continue
		}
		var rec upkeepDispositionPayload
		if json.Unmarshal(e.Payload, &rec) != nil || rec.FindingID == "" || rec.ArtifactID != artifactID {
			continue
		}
		if prev, ok := byID[rec.FindingID]; ok && prev.AuditSequence > e.Sequence {
			continue
		}
		subject := ""
		if e.ActorSubject != nil {
			subject = *e.ActorSubject
		}
		byID[rec.FindingID] = recordedUpkeepDisposition{
			FindingID: rec.FindingID, Source: rec.Source, Verdict: rec.Verdict,
			AuthorizeDelegationTier: rec.AuthorizeDelegationTier, ParentEpic: rec.ParentEpic,
			RecordedAt: e.Timestamp.UTC().Format(time.RFC3339Nano),
			RecordedBy: subject, AuditSequence: e.Sequence,
		}
	}
	out := make([]recordedUpkeepDisposition, 0, len(byID))
	for _, d := range byID {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FindingID < out[j].FindingID })
	return out
}
