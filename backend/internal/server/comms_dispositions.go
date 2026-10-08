package server

// Per-draft comms disposition CAPTURE and the captain's dispositions READ
// (E81.5 / #3775, phase 6 #4016).
//
// The captain records a verdict — approved / rejected — against an individual
// comms-report DRAFT, keyed by its derived draft id, optionally with a
// parent_epic override on an approved draft. Each accepted entry is ONE
// comms_disposition_recorded row; the comms apply (phase 7, #4017) consumes
// them.
//
// It is the upkeep capture (upkeep_dispositions.go) on the shared
// proposal-report seam (report_seam.go): the same operator-only ladder
// (requireOperatorCapture), the STRICT single-document decode
// (decodeStrictSingleJSONBody), batch-atomic validation, last-wins read-back,
// and the capture/apply WINDOW, driven through the GENERIC
// audit.FamilyWindowAppender with the comms family (watermark
// comms_apply_window_closed, binding re-check over comms_report_recorded).
//
// THE BINDING RULE: dispositions bind to the artifact named by the
// HIGHEST-sequence comms_report_recorded row on the run's chain
// (latestCommsReport), never the newest artifact. The atomic append RE-CHECKS
// that binding under the run-row lock and refuses 409 comms_report_superseded
// when a newer report was recorded after resolution.
//
// THE READ is the captain's review surface: both verbs return each draft's
// RECORDED preview (the final rendered filing body as the ingest recorded it,
// never the raw artifact, whose agent prose is unneutralized), the undecided
// drafts, the unaccounted report ids and the cluster splits. Contract:
// docs/spec/comms-report-v1.md § "Dispositions".

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
)

// CategoryCommsDispositionRecorded aliases the audit-layer constant so the
// capture handler and the window protocol's consumed-set scan share one value.
const CategoryCommsDispositionRecorded = audit.CommsDispositionRecordedCategory

// commsMaxDispositions bounds one capture batch: the report's drafts maxItems
// (plan.CommsMaxDrafts, pinned to docs/spec/comms-report-v1.schema.json), so a
// batch can name every draft once and no more.
const commsMaxDispositions = plan.CommsMaxDrafts

// Comms verdicts: the CLOSED set a disposition must name.
const (
	commsVerdictApproved = "approved"
	commsVerdictRejected = "rejected"
)

var commsVerdictNames = []string{commsVerdictApproved, commsVerdictRejected}

// The route's own error codes and the details.reason of its
// parent_epic_is_source refusal.
const (
	commsDispositionsUnconfiguredCode = "comms_dispositions_unconfigured"
	commsVerdictInvalidCode           = "comms_verdict_invalid"
	commsReportAbsentCode             = "comms_report_absent"
	commsDraftUnknownCode             = "comms_draft_unknown"
	commsWindowClosedCode             = "comms_window_closed"
	commsReportSupersededCode         = "comms_report_superseded"
)

// commsDispositionsCodes is the canonical, ordered list of EVERY error code
// either verb can answer. docs/spec/comms-report-v1.md § "Dispositions"
// carries one table row per entry (comms_dispositions_docs_test.go requires
// set-equality both ways), and TestCommsDispositions_EveryCodeReachable drives
// a case per entry.
var commsDispositionsCodes = []string{
	commsDispositionsUnconfiguredCode,
	"authentication_required",
	"run_token_forbidden",
	"operator_agent_forbidden",
	"insufficient_scope",
	"validation_failed",
	"run_not_found",
	commsVerdictInvalidCode,
	commsReportAbsentCode,
	commsDraftUnknownCode,
	commsWindowClosedCode,
	commsReportSupersededCode,
	"internal_error",
}

// commsOperatorOnly is the comms capture's operator-only ladder text.
var commsOperatorOnly = operatorOnlyMessages{
	runToken:      "a run-bound agent token may not record a comms disposition; deciding a comms draft is a captain action",
	operatorAgent: "a delegated operator-agent token may not record a comms disposition; the comms report is agent-authored, so an agent recording the verdict would convert the captain gate into a self-approval",
}

// commsDispositionInput is one requested disposition. ParentEpic is a pointer
// so a present-but-empty value is validated (and refused) rather than read as
// absent.
type commsDispositionInput struct {
	DraftID    string  `json:"draft_id"`
	Verdict    string  `json:"verdict"`
	ParentEpic *string `json:"parent_epic,omitempty"`
}

type commsDispositionRequest struct {
	Dispositions []commsDispositionInput `json:"dispositions"`
}

// commsDispositionPayload is the comms_disposition_recorded payload, marshalled
// BARE so the audit layer reads artifact_id straight off it. parent_epic is
// present only when the captain supplied one.
type commsDispositionPayload struct {
	RunID       string `json:"run_id"`
	StageID     string `json:"stage_id"`
	ArtifactID  string `json:"artifact_id"`
	ContentHash string `json:"content_hash"`
	DraftID     string `json:"draft_id"`
	Verdict     string `json:"verdict"`
	ParentEpic  string `json:"parent_epic,omitempty"`
}

// recordedCommsDisposition is one projected read-back row.
type recordedCommsDisposition struct {
	DraftID       string `json:"draft_id"`
	Verdict       string `json:"verdict"`
	ParentEpic    string `json:"parent_epic,omitempty"`
	RecordedAt    string `json:"recorded_at"`
	RecordedBy    string `json:"recorded_by"`
	AuditSequence int64  `json:"audit_sequence"`
}

// commsClusterPlacement is where ONE member of a recorded cluster landed in
// the report: kind draft (EntryID the citing draft id), n_drift (EntryID the
// n_drift id), not_drafted (EntryID the not_drafted reason) or unaccounted
// (cited nowhere; no EntryID).
type commsClusterPlacement struct {
	ReportID string `json:"report_id"`
	Kind     string `json:"kind"`
	EntryID  string `json:"entry_id,omitempty"`
}

// The placement kinds.
const (
	commsPlacementDraft       = "draft"
	commsPlacementNDrift      = "n_drift"
	commsPlacementNotDrafted  = "not_drafted"
	commsPlacementUnaccounted = "unaccounted"
)

// commsClusterSplit is one server-suggested cluster the report did NOT keep
// together: its members did not all land in the same placement.
type commsClusterSplit struct {
	ReportIDs  []string                `json:"report_ids"`
	Score      float64                 `json:"score"`
	Placements []commsClusterPlacement `json:"placements"`
}

// commsDispositionsResponse is the body BOTH verbs return.
//
// Previews is JSON-EQUAL to the recorded row's previews value (approval
// condition 3), RE-ENCODED, not byte-identical: Postgres JSONB normalizes key
// order and whitespace on store, and encoding/json re-escapes the raw value on
// the way out (`&`, `<`, `>` become &, <, >). It is kept as
// json.RawMessage and never decoded into a typed shape, so no field is
// dropped or renamed on the way to the captain, and every decoded value (a
// preview body included) equals the one recorded.
type commsDispositionsResponse struct {
	RunID                string                     `json:"run_id"`
	ArtifactID           string                     `json:"artifact_id"`
	StageID              string                     `json:"stage_id"`
	ContentHash          string                     `json:"content_hash"`
	GatherDigest         string                     `json:"gather_digest"`
	WindowClosed         bool                       `json:"window_closed"`
	Settlement           *groomingWindowSettlement  `json:"settlement,omitempty"`
	Dispositions         []recordedCommsDisposition `json:"dispositions"`
	UndecidedDraftIDs    []string                   `json:"undecided_draft_ids"`
	Previews             json.RawMessage            `json:"previews"`
	PreviewDegraded      bool                       `json:"preview_degraded"`
	PreviewDegradeReason string                     `json:"preview_degrade_reason,omitempty"`
	CharterText          string                     `json:"charter_text"`
	UnaccountedReportIDs []string                   `json:"unaccounted_report_ids"`
	ClustersRecorded     bool                       `json:"clusters_recorded"`
	ClusterSplits        []commsClusterSplit        `json:"cluster_splits"`
}

// commsRecordedRow is the subset of a comms_report_recorded row (written by
// commsRecordedPayload) the dispositions read needs. It is decoded with plain
// json.Unmarshal (approval condition 1): the row carries more keys than this
// (run_id, schema_version, entry_counts, gather_sequence, …) and may gain
// more, so a strict decode would refuse every real row. Previews stays RAW.
// The pointer fields tell an absent (or null) key from an empty one.
type commsRecordedRow struct {
	GatherDigest         string          `json:"gather_digest"`
	UnaccountedReportIDs *[]string       `json:"unaccounted_report_ids"`
	Previews             json.RawMessage `json:"previews"`
	PreviewDegraded      bool            `json:"preview_degraded"`
	PreviewDegradeReason string          `json:"preview_degrade_reason"`
	CharterText          string          `json:"charter_text"`
}

// decodeCommsRecordedRow leniently decodes a comms_report_recorded payload and
// requires the keys the read cannot do without: a non-empty gather_digest, a
// previews ARRAY and an unaccounted_report_ids ARRAY. A row missing any of them
// is an error (500), never an empty view: an absent previews key read as "no
// previews", or an absent unaccounted list read as "nothing unaccounted", would
// tell the captain something the row never said.
func decodeCommsRecordedRow(e *audit.Entry) (*commsRecordedRow, error) {
	var row commsRecordedRow
	if err := json.Unmarshal(e.Payload, &row); err != nil {
		return nil, fmt.Errorf("decode %s row %d: %w", CategoryCommsReportRecorded, e.Sequence, err)
	}
	if row.GatherDigest == "" {
		return nil, fmt.Errorf("%s row %d carries no gather_digest", CategoryCommsReportRecorded, e.Sequence)
	}
	var previews []json.RawMessage
	if row.Previews == nil || json.Unmarshal(row.Previews, &previews) != nil || previews == nil {
		return nil, fmt.Errorf("%s row %d carries no previews array", CategoryCommsReportRecorded, e.Sequence)
	}
	if row.UnaccountedReportIDs == nil {
		return nil, fmt.Errorf("%s row %d carries no unaccounted_report_ids array", CategoryCommsReportRecorded, e.Sequence)
	}
	return &row, nil
}

// errCommsReportAbsent: the run's chain carries no comms_report_recorded row —
// distinct from a read/parse FAILURE, which is a wrapped error.
var errCommsReportAbsent = errors.New("run carries no recorded comms_report")

// commsReportBinding is the resolved report a capture attaches to: the
// artifact, its parsed report, the recorded row, and the EXACT gather that row
// names (its audit entry kept for the clusters-recorded probe).
type commsReportBinding struct {
	art         *artifact.Artifact
	report      *plan.CommsReport
	stageID     uuid.UUID
	recorded    *commsRecordedRow
	gather      *commsScanGatheredPayload
	gatherEntry *audit.Entry
}

// latestCommsReport resolves the report dispositions bind to: the artifact
// named by the HIGHEST-sequence comms_report_recorded row on the run's chain,
// never the newest artifact. The comms apply (phase 7, #4017) MUST resolve
// through this same function.
//
// It also loads the gather that row names EXACTLY by its stored gather_digest
// (commsScanGatheredByDigest). The stored digest is authoritative: no path
// recomputes a digest from a decoded gather and compares it (a row recorded
// before suggested_clusters existed no longer re-digests to its stored value).
//
// Outcomes stay distinct: no recorded row → errCommsReportAbsent (an orphan
// artifact whose recorded row never landed is NOT bindable); an undecodable
// newest row, an unreadable artifact, a wrong kind, a parse failure, or a
// gather that cannot be loaded or decoded → a wrapped error (500), never a
// silent fallback to an older report.
func (s *Server) latestCommsReport(ctx context.Context, runID uuid.UUID) (*commsReportBinding, error) {
	newest, err := s.recordedReportRow(ctx, runID, CategoryCommsReportRecorded, "")
	if err != nil {
		var le *reportRowsListError
		if errors.As(err, &le) {
			return nil, fmt.Errorf("list %s rows for run %s: %w", CategoryCommsReportRecorded, runID, le.Err)
		}
		return nil, err
	}
	if newest == nil {
		return nil, errCommsReportAbsent
	}
	artID, perr := recordedRowArtifactID(newest, CategoryCommsReportRecorded)
	if perr != nil {
		return nil, perr
	}
	row, derr := decodeCommsRecordedRow(newest)
	if derr != nil {
		return nil, derr
	}
	art, gerr := s.cfg.ArtifactRepo.Get(ctx, artID)
	if gerr != nil {
		return nil, fmt.Errorf("read comms_report artifact %s: %w", artID, gerr)
	}
	if art.Kind != artifact.KindCommsReport {
		return nil, fmt.Errorf("artifact %s named by %s row %d has kind %q, want %q", artID, CategoryCommsReportRecorded, newest.Sequence, art.Kind, artifact.KindCommsReport)
	}
	report, rerr := plan.ParseCommsReport(art.Content)
	if rerr != nil {
		return nil, fmt.Errorf("parse comms_report artifact %s: %w", artID, rerr)
	}
	gEntry, gathered, gherr := s.commsScanGatheredByDigest(ctx, runID, row.GatherDigest)
	if gherr != nil {
		return nil, fmt.Errorf("load the gather %s row %d names: %w", CategoryCommsReportRecorded, newest.Sequence, gherr)
	}
	return &commsReportBinding{
		art: art, report: report, stageID: art.StageID,
		recorded: row, gather: gathered, gatherEntry: gEntry,
	}, nil
}

// writeCommsReportResolveError maps latestCommsReport's outcomes.
func (s *Server) writeCommsReportResolveError(w http.ResponseWriter, r *http.Request, runID uuid.UUID, rerr error) {
	if errors.Is(rerr, errCommsReportAbsent) {
		s.writeError(w, r, http.StatusConflict, commsReportAbsentCode,
			"this run has no recorded comms_report; there is nothing to disposition",
			map[string]any{"run_id": runID.String()})
		return
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
		"comms-dispositions: resolving the recorded comms_report failed",
		slog.String("run_id", runID.String()), slog.String("error", rerr.Error()))
	s.writeError(w, r, http.StatusInternalServerError, "internal_error",
		"the run's recorded comms_report, or the gather it names, could not be read or parsed",
		map[string]any{"error": rerr.Error()})
}

// commsUnconfigured is the C0 rung shared by both verbs.
func (s *Server) commsUnconfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, commsDispositionsUnconfiguredCode,
			"comms-dispositions endpoint requires run + artifact + audit repositories", nil)
		return true
	}
	return false
}

// handleRecordCommsDispositions implements
// POST /v0/runs/{run_id}/comms-dispositions.
//
// EVERY rung runs BEFORE any write, and the WHOLE batch is validated before
// ANY row is appended:
//
//	C0  503 comms_dispositions_unconfigured
//	C1  401 authentication_required
//	C2  403 run_token_forbidden
//	C3  403 operator_agent_forbidden
//	C4  403 insufficient_scope (write:approvals, unconditional)
//	C5  400 validation_failed — bad run_id
//	C6  404 run_not_found
//	C7  400 validation_failed — unparseable body, an unknown key at any depth,
//	        a wrongly-typed value, trailing content, empty batch, > 25
//	        entries, empty or duplicate draft_id, a parent_epic
//	        plan.CommsParentEpicNumber cannot parse, a parent_epic on a
//	        rejected verdict
//	C8  400 comms_verdict_invalid — verdict outside {approved, rejected}
//	C9  409 comms_report_absent / 500 unreadable report or gather
//	C10 422 comms_draft_unknown — whole-batch check
//	C11 400 validation_failed reason parent_epic_is_source
//	C12 409 comms_window_closed; 409 comms_report_superseded (atomic path)
//
// The 500s after C11 are DISTINCT and carry details.recorded /
// details.requested: an atomic batch failure recorded NOTHING (recorded=0); a
// read-back failure AFTER the batch committed means EVERY row is durable
// (recorded=requested); the fallback's mid-batch failure reports the partial
// count. A repeat POST is safe in all three, because capture is last-wins.
func (s *Server) handleRecordCommsDispositions(w http.ResponseWriter, r *http.Request) {
	if s.commsUnconfigured(w, r) { // C0
		return
	}
	id, ok := s.requireOperatorCapture(w, r, commsOperatorOnly) // C1..C4
	if !ok {
		return
	}
	runID, ok := s.upkeepParseRunID(w, r) // C5
	if !ok {
		return
	}
	if !s.upkeepRequireRun(w, r, runID) { // C6
		return
	}

	// C7 / C8: body shape and per-entry validation.
	var reqBody commsDispositionRequest
	if !s.decodeStrictSingleJSONBody(w, r, &reqBody,
		"request body must be valid JSON {dispositions:[{draft_id, verdict, parent_epic}]} with no other keys; an unknown or misspelled key is refused rather than dropped",
		"request body must be a single JSON document; trailing content after the dispositions object is refused because a decoder that stopped at the first value would silently discard it and report success") {
		return
	}
	if len(reqBody.Dispositions) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"dispositions must name at least one draft; an empty capture records nothing and is ambiguous intent",
			map[string]any{"field": "dispositions"})
		return
	}
	if n := len(reqBody.Dispositions); n > commsMaxDispositions {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("dispositions names %d entries; at most %d (the report's drafts maxItems) are allowed", n, commsMaxDispositions),
			map[string]any{"field": "dispositions", "max": commsMaxDispositions, "got": n})
		return
	}
	seen := make(map[string]struct{}, len(reqBody.Dispositions))
	for i := range reqBody.Dispositions {
		d := &reqBody.Dispositions[i]
		field := func(name string) string { return fmt.Sprintf("dispositions[%d].%s", i, name) }
		d.DraftID = strings.TrimSpace(d.DraftID)
		if d.DraftID == "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"draft_id is required on every disposition", map[string]any{"field": field("draft_id")})
			return
		}
		if _, dup := seen[d.DraftID]; dup {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"draft_id appears more than once in this batch; one request carrying two verdicts for one draft is ambiguous — record the correcting verdict as a separate request, which supersedes the earlier one",
				map[string]any{"field": field("draft_id"), "draft_id": d.DraftID})
			return
		}
		seen[d.DraftID] = struct{}{}

		d.Verdict = strings.TrimSpace(d.Verdict)
		if d.Verdict != commsVerdictApproved && d.Verdict != commsVerdictRejected { // C8
			s.writeError(w, r, http.StatusBadRequest, commsVerdictInvalidCode,
				"verdict must name one of the comms verdicts",
				map[string]any{"field": field("verdict"), "got": d.Verdict, "allowed": commsVerdictNames})
			return
		}
		if d.ParentEpic != nil {
			// The ONE parent_epic parser (approval condition 4), shared with
			// the C11 rung and the ingest.
			if _, ok := plan.CommsParentEpicNumber(*d.ParentEpic); !ok {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"parent_epic must be a positive issue number, bare (389) or #-prefixed (#389)",
					map[string]any{"field": field("parent_epic"), "got": *d.ParentEpic})
				return
			}
			if d.Verdict == commsVerdictRejected {
				// A rejected draft files nothing, so an override on it is
				// contradictory intent, not a no-op to record.
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"parent_epic is an override for the issue an APPROVED draft files; a rejected draft files nothing",
					map[string]any{"field": field("parent_epic")})
				return
			}
		}
	}

	// C9: resolve the binding. Absent and unreadable stay DISTINCT.
	b, rerr := s.latestCommsReport(r.Context(), runID)
	if rerr != nil {
		s.writeCommsReportResolveError(w, r, runID, rerr)
		return
	}

	// C10: every draft id must be one the report declares — checked for the
	// WHOLE batch before any append, which is what makes capture batch-atomic.
	drafts := make(map[string]bool, len(b.report.Drafts))
	for _, d := range b.report.Drafts {
		drafts[d.ID] = true
	}
	var unknown []string
	for _, d := range reqBody.Dispositions {
		if !drafts[d.DraftID] {
			unknown = append(unknown, d.DraftID)
		}
	}
	if len(unknown) > 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, commsDraftUnknownCode,
			"one or more draft_id values are not declared by this run's recorded comms_report; NO disposition was recorded",
			map[string]any{"unknown_draft_ids": unknown, "artifact_id": b.art.ID.String()})
		return
	}

	// C11: an approved draft's parent_epic must not be an issue its own cited
	// reports live on (the ingest's rule, over the captain's override).
	if ref := commsParentEpicIsSource(b.report, b.gather, reqBody.Dispositions); ref != nil {
		details := map[string]any{}
		for k, v := range ref.Details {
			details[k] = v
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", ref.Message, details)
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
		pl := commsDispositionPayload{
			RunID: runID.String(), StageID: stageID.String(),
			ArtifactID: b.art.ID.String(), ContentHash: b.art.ContentHash,
			DraftID: d.DraftID, Verdict: d.Verdict,
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
			Category:  CategoryCommsDispositionRecorded,
			ActorKind: &actorKind, ActorSubject: &subject, Payload: payload,
		})
	}

	if appender, ok := s.cfg.AuditRepo.(audit.FamilyWindowAppender); ok {
		// ATOMIC BATCH: one capture is one transaction under the run-row lock,
		// which re-checks the binding and the window before appending.
		_, aerr := appender.AppendChainedFamilyDispositionBatch(r.Context(), audit.WindowFamilyComms, b.art.ID.String(), params)
		var (
			closed     *audit.WindowClosedError
			superseded *audit.ReportSupersededError
		)
		switch {
		case errors.As(aerr, &superseded):
			s.writeError(w, r, http.StatusConflict, commsReportSupersededCode,
				"a newer comms_report was recorded on this run after this capture resolved its report; NO disposition was recorded — re-read the dispositions view and re-capture against the current report",
				map[string]any{
					"artifact_id":         superseded.ArtifactID,
					"current_artifact_id": superseded.CurrentArtifactID,
					"current_sequence":    superseded.CurrentSequence,
				})
			return
		case errors.As(aerr, &closed):
			s.writeCommsWindowClosed(w, r, closed.ArtifactID, closed.Settlement, closed.Sequence)
			return
		case aerr != nil:
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"comms-dispositions: atomic disposition batch failed",
				slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"recording the disposition batch failed; the capture is atomic, so NOTHING was recorded and a repeat POST is safe",
				map[string]any{"recorded": 0, "requested": len(params), "error": aerr.Error()})
			return
		}
	} else {
		// FALLBACK (in-memory repos without the capability): check the window,
		// then a per-row loop. NON-ATOMIC — no in-transaction binding re-check,
		// and a mid-batch failure can leave durable partial rows; `recorded` /
		// `requested` are the operator's evidence of what survived. Production
		// always takes the atomic path (postgres.go assertion + decisionindex
		// forward).
		settlement, serr := s.windowSettlementFor(r.Context(), runID, audit.CommsApplyWindowClosedCategory, b.art.ID.String())
		if serr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"listing the comms capture window failed", map[string]any{"error": serr.Error()})
			return
		}
		if settlement != nil {
			s.writeCommsWindowClosed(w, r, b.art.ID.String(), settlement.Settlement, settlement.AuditSequence)
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

	s.respondCommsDispositions(w, r, runID, b, len(params))
}

// commsParentEpicIsSource is the C11 decision: the first APPROVED entry whose
// parent_epic equals the issue number a report its draft cites lives on
// (comment reports included), from the bound gather's shown entries — the
// ingest's own computation (checkCommsCharterRefs). A parent_epic
// plan.CommsParentEpicNumber cannot parse is ALSO a refusal here, never a
// silent skip of the equality check (approval condition 4); C7 already refuses
// it, so that leg is defence in depth. nil when no entry is refused.
func commsParentEpicIsSource(report *plan.CommsReport, gathered *commsScanGatheredPayload, entries []commsDispositionInput) *commsRefRefusal {
	issueOf := make(map[string]int, len(gathered.Shown))
	for _, sr := range gathered.Shown {
		issueOf[sr.ID] = sr.IssueNumber
	}
	sources := make(map[string][]string, len(report.Drafts))
	for _, d := range report.Drafts {
		sources[d.ID] = d.SourceReportIDs
	}
	for i, d := range entries {
		if d.Verdict != commsVerdictApproved || d.ParentEpic == nil {
			continue
		}
		field := fmt.Sprintf("dispositions[%d].parent_epic", i)
		epic, ok := plan.CommsParentEpicNumber(*d.ParentEpic)
		if !ok {
			return &commsRefRefusal{"",
				"parent_epic must be a positive issue number, bare (389) or #-prefixed (#389)",
				map[string]any{"field": field, "got": *d.ParentEpic}}
		}
		for _, id := range sources[d.DraftID] {
			if n, shown := issueOf[id]; shown && n == epic {
				return &commsRefRefusal{commsRefParentEpicIsSource,
					fmt.Sprintf("draft %s would file under parent_epic %s, the issue its cited report %s lives on; a report cannot be its own epic", d.DraftID, *d.ParentEpic, id),
					map[string]any{"reason": commsRefParentEpicIsSource, "field": field, "draft_id": d.DraftID, "parent_epic": *d.ParentEpic}}
			}
		}
	}
	return nil
}

// writeCommsWindowClosed is the C12 window refusal, shared by both append
// paths.
func (s *Server) writeCommsWindowClosed(w http.ResponseWriter, r *http.Request, artifactID, settlement string, seq int64) {
	s.writeError(w, r, http.StatusConflict, commsWindowClosedCode,
		"this comms report's disposition-capture window has been settled by the apply; NO disposition was recorded and the dispositions you sent are not consumable",
		map[string]any{"artifact_id": artifactID, "settlement": settlement, "watermark_sequence": seq})
}

// handleListCommsDispositions implements
// GET /v0/runs/{run_id}/comms-dispositions: read access only (the operator-only
// posture is scoped to capture). It answers C0, C5, C6 and C9 like the POST.
func (s *Server) handleListCommsDispositions(w http.ResponseWriter, r *http.Request) {
	if s.commsUnconfigured(w, r) {
		return
	}
	runID, ok := s.upkeepParseRunID(w, r)
	if !ok {
		return
	}
	if !s.upkeepRequireRun(w, r, runID) {
		return
	}
	b, rerr := s.latestCommsReport(r.Context(), runID)
	if rerr != nil {
		s.writeCommsReportResolveError(w, r, runID, rerr)
		return
	}
	s.respondCommsDispositions(w, r, runID, b, 0)
}

// respondCommsDispositions writes the 200 shared by both verbs, so the POST
// echo and the GET read-back are the same bytes by construction. committed is
// the number of rows the POST's batch has ALREADY made durable (0 on the GET):
// a read-back failure there must not read as "nothing recorded", so its 500
// says the batch landed (details.recorded = requested).
func (s *Server) respondCommsDispositions(w http.ResponseWriter, r *http.Request, runID uuid.UUID, b *commsReportBinding, committed int) {
	readBackFailed := func(what string, err error) {
		if committed == 0 {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				what+" failed", map[string]any{"error": err.Error()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the disposition batch WAS recorded and is durable, but "+what+" for the read-back failed; re-read with GET, or repeat the POST (safe: capture is last-wins)",
			map[string]any{"recorded": committed, "requested": committed, "error": err.Error()})
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryCommsDispositionRecorded)
	if err != nil {
		readBackFailed("listing recorded dispositions", err)
		return
	}
	settlement, serr := s.windowSettlementFor(r.Context(), runID, audit.CommsApplyWindowClosedCategory, b.art.ID.String())
	if serr != nil {
		readBackFailed("listing the comms capture window", serr)
		return
	}
	dispositions := projectCommsDispositions(entries, b.art.ID.String())
	decided := make(map[string]bool, len(dispositions))
	for _, d := range dispositions {
		decided[d.DraftID] = true
	}
	undecided := []string{}
	for _, d := range b.report.Drafts {
		if !decided[d.ID] {
			undecided = append(undecided, d.ID)
		}
	}
	recorded := commsGatheredClustersRecorded(b.gatherEntry.Payload)
	splits := []commsClusterSplit{}
	if recorded {
		splits = commsClusterSplits(b.report, b.gather.SuggestedClusters)
	}
	s.writeJSON(w, r, http.StatusOK, commsDispositionsResponse{
		RunID:                runID.String(),
		ArtifactID:           b.art.ID.String(),
		StageID:              b.stageID.String(),
		ContentHash:          b.art.ContentHash,
		GatherDigest:         b.recorded.GatherDigest,
		WindowClosed:         settlement != nil,
		Settlement:           settlement,
		Dispositions:         dispositions,
		UndecidedDraftIDs:    undecided,
		Previews:             b.recorded.Previews,
		PreviewDegraded:      b.recorded.PreviewDegraded,
		PreviewDegradeReason: b.recorded.PreviewDegradeReason,
		CharterText:          b.recorded.CharterText,
		UnaccountedReportIDs: *b.recorded.UnaccountedReportIDs,
		ClustersRecorded:     recorded,
		ClusterSplits:        splits,
	})
}

// projectCommsDispositions collapses the run's comms_disposition_recorded rows
// into the current set for ONE artifact: undecodable rows are SKIPPED (a junk
// row must not manufacture a verdict), rows of another artifact are excluded,
// repeats collapse LAST-WINS by audit sequence (both rows stay in the chain),
// and output is sorted by draft_id.
func projectCommsDispositions(entries []*audit.Entry, artifactID string) []recordedCommsDisposition {
	byID := make(map[string]recordedCommsDisposition)
	for _, e := range entries {
		if e == nil {
			continue
		}
		var rec commsDispositionPayload
		if json.Unmarshal(e.Payload, &rec) != nil || rec.DraftID == "" || rec.ArtifactID != artifactID {
			continue
		}
		if prev, ok := byID[rec.DraftID]; ok && prev.AuditSequence > e.Sequence {
			continue
		}
		subject := ""
		if e.ActorSubject != nil {
			subject = *e.ActorSubject
		}
		byID[rec.DraftID] = recordedCommsDisposition{
			DraftID: rec.DraftID, Verdict: rec.Verdict, ParentEpic: rec.ParentEpic,
			RecordedAt: e.Timestamp.UTC().Format(time.RFC3339Nano),
			RecordedBy: subject, AuditSequence: e.Sequence,
		}
	}
	out := make([]recordedCommsDisposition, 0, len(byID))
	for _, d := range byID {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DraftID < out[j].DraftID })
	return out
}

// commsClusterSplits returns the recorded clusters the report did NOT keep
// together, in the gather's order. Each member is placed (commsReportPlacement)
// and a cluster is SPLIT when its members' (kind, entry_id) placements are not
// all equal; a cluster kept whole is omitted. Always non-nil.
func commsClusterSplits(report *plan.CommsReport, clusters []commsRecordedCluster) []commsClusterSplit {
	place := commsReportPlacement(report)
	out := []commsClusterSplit{}
	for _, c := range clusters {
		placements := make([]commsClusterPlacement, 0, len(c.ReportIDs))
		split := false
		for i, id := range c.ReportIDs {
			p, ok := place[id]
			if !ok {
				p = commsClusterPlacement{Kind: commsPlacementUnaccounted}
			}
			p.ReportID = id
			placements = append(placements, p)
			if i > 0 && (p.Kind != placements[0].Kind || p.EntryID != placements[0].EntryID) {
				split = true
			}
		}
		if !split {
			continue
		}
		out = append(out, commsClusterSplit{
			ReportIDs:  append([]string{}, c.ReportIDs...),
			Score:      c.Score,
			Placements: placements,
		})
	}
	return out
}

// commsReportPlacement maps every report id the report cites to its placement
// (ReportID unset). The semantic rules let a report id appear in at most ONE
// entry, so each id has one placement.
func commsReportPlacement(report *plan.CommsReport) map[string]commsClusterPlacement {
	out := map[string]commsClusterPlacement{}
	for _, d := range report.Drafts {
		for _, id := range d.SourceReportIDs {
			out[id] = commsClusterPlacement{Kind: commsPlacementDraft, EntryID: d.ID}
		}
	}
	for _, n := range report.NDrift {
		for _, id := range n.SourceReportIDs {
			out[id] = commsClusterPlacement{Kind: commsPlacementNDrift, EntryID: n.ID}
		}
	}
	for _, nd := range report.NotDrafted {
		out[nd.ReportID] = commsClusterPlacement{Kind: commsPlacementNotDrafted, EntryID: nd.Reason}
	}
	return out
}
