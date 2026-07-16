package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gateViewResponse is GET /v0/runs/{run_id}/gate-view (#1960): the
// gate-scoped decision read that answers "what is still open at this gate
// and why" in ONE call. It carries each OPEN concern with its FULL note
// prose (unlike the run-status concerns block, which elides the note), the
// per-concern cross-round history reconstructed from the immutable audit
// payloads (fix-up routing claims + re-review confirmations), the settled
// ledger, and the run's suppressed relitigations.
//
// History is reconstructed from audit payloads because concern.StateReason
// is OVERWRITTEN on every transition (MarkAddressedPending writes the
// routing reason, then applyConcernResolutions overwrites it with the
// re-review note) — there is no stored per-round history. Degradation is
// VISIBLE, never silent: when the audit reads are unavailable the concerns
// are returned intact with HistoryIncomplete=true and HistoryGaps naming
// each unreadable category.
type gateViewResponse struct {
	RunID uuid.UUID `json:"run_id"`
	// StageKind echoes the optional stage_kind filter (plan|implement). Empty
	// when the caller asked for the whole run.
	StageKind               string                      `json:"stage_kind,omitempty"`
	Open                    []gateViewConcern           `json:"open"`
	Settled                 []gateViewSettledConcern    `json:"settled"`
	SuppressedRelitigations []gateViewSuppressedRelitig `json:"suppressed_relitigations"`
	// HistoryIncomplete is true when one or more audit-derived history joins
	// could not be built (AuditRepo unconfigured, or a per-category read
	// error). The concerns themselves are always intact — only the fixups[]
	// / resolutions[] / suppressed_relitigations[] cross-references may be
	// missing. HistoryGaps names each category that failed.
	HistoryIncomplete bool     `json:"history_incomplete"`
	HistoryGaps       []string `json:"history_gaps,omitempty"`
}

// gateViewConcern is one OPEN concern with its full decision context.
type gateViewConcern struct {
	ID        uuid.UUID `json:"id"`
	StageKind string    `json:"stage_kind"`
	// Round is the round-of-origin for an implement concern: 1 + the count of
	// same-stage stage_fixup_triggered entries below the origin review
	// sequence (the latestRoundConcerns convention). Omitted for plan
	// concerns, which have no fix-up rounds.
	Round                int    `json:"round,omitempty"`
	OriginReviewSequence int64  `json:"origin_review_sequence"`
	ReviewerModel        string `json:"reviewer_model,omitempty"`
	Severity             string `json:"severity"`
	Category             string `json:"category"`
	State                string `json:"state"`
	StateReason          string `json:"state_reason,omitempty"`
	// Note is the FULL reviewer prose — deliberately NOT elided (the whole
	// point of this surface). The MCP tool passes it through with none of the
	// compaction levers applied.
	Note              string               `json:"note"`
	HasSuggestedPatch bool                 `json:"has_suggested_patch"`
	Fixups            []gateViewFixup      `json:"fixups,omitempty"`
	Resolutions       []gateViewResolution `json:"resolutions,omitempty"`
}

// gateViewFixup is one fix-up routing claim reconstructed from the
// stage_fixup_triggered audit entry that named this concern, joined to the
// outcome from the earliest following fixup_pushed / fixup_no_changes entry.
type gateViewFixup struct {
	Sequence int64  `json:"sequence"`
	Reason   string `json:"reason,omitempty"`
	// Outcome is "pushed" (a fix-up commit landed), "no_changes" (the pass
	// produced nothing), or "pending" (the trigger has no following outcome
	// entry yet).
	Outcome   string `json:"outcome"`
	ApplyPath string `json:"apply_path,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
}

// gateViewResolution is one re-review verdict on this concern, reconstructed
// from an implement_reviewed / plan_reviewed entry's concern_resolutions.
type gateViewResolution struct {
	Sequence int64 `json:"sequence"`
	// Round is derived for an implement re-review (as for gateViewConcern.Round);
	// omitted for a plan re-review.
	Round      int    `json:"round,omitempty"`
	Resolution string `json:"resolution"`
	Note       string `json:"note,omitempty"`
}

// gateViewSettledConcern is one row of the settled ledger (#1913): a concern
// in a terminal state (addressed / waived / superseded / deferred) with its
// state_reason.
type gateViewSettledConcern struct {
	ID            uuid.UUID `json:"id"`
	StageKind     string    `json:"stage_kind"`
	State         string    `json:"state"`
	Severity      string    `json:"severity"`
	Category      string    `json:"category"`
	ReviewerModel string    `json:"reviewer_model,omitempty"`
	Note          string    `json:"note"`
	StateReason   string    `json:"state_reason,omitempty"`
}

// gateViewSuppressedRelitig mirrors concernRelitigationSuppressedPayload on
// the wire: a re-raise of a settled concern the reviewer would have relitigated,
// suppressed and recorded (#1913).
type gateViewSuppressedRelitig struct {
	SettledRef           string `json:"settled_ref"`
	SettledState         string `json:"settled_state"`
	Severity             string `json:"severity"`
	Category             string `json:"category"`
	Note                 string `json:"note"`
	ReviewerModel        string `json:"reviewer_model,omitempty"`
	OriginReviewSequence int64  `json:"origin_review_sequence"`
}

// gateViewFixupTriggerPayload is the subset of a stage_fixup_triggered audit
// payload the join reads: which concerns were routed and the operator/driver
// reason. See handleFixupStage's fields map (fixup.go).
type gateViewFixupTriggerPayload struct {
	ConcernIDs []string `json:"concern_ids"`
	Reason     string   `json:"reason"`
}

// gateViewFixupOutcomePayload is the subset of a fixup_pushed / fixup_no_changes
// audit payload the join reads. apply_path is present only on fixup_pushed.
type gateViewFixupOutcomePayload struct {
	HeadSHA   string `json:"head_sha"`
	ApplyPath string `json:"apply_path"`
}

// gateViewResolutionsPayload is the subset of an implement_reviewed /
// plan_reviewed audit payload the join reads: the reviewer's delta-verification
// verdicts on prior concerns (planreview.ConcernResolution shape).
type gateViewResolutionsPayload struct {
	ConcernResolutions []struct {
		ID         string `json:"id"`
		Resolution string `json:"resolution"`
		Note       string `json:"note"`
	} `json:"concern_resolutions"`
}

// gateViewHistoryCategories is the set of audit categories the history join
// reads, in a stable order. A read failure on any one degrades visibly
// (history_incomplete + a gap naming it) rather than failing the response.
var gateViewHistoryCategories = []string{
	CategoryStageFixupTriggered,
	"fixup_pushed",
	"fixup_no_changes",
	"implement_reviewed",
	"plan_reviewed",
	concernRelitigationSuppressedCategory,
}

// scopeGateViewRead is the read scope a non-mcp caller must hold to read the
// gate view (#1960). It mirrors handleListRunAudit's audit-read posture: the
// gate view is reconstructed from the run's immutable audit history, and its
// FULL reviewer prose — which can carry sensitive repository context under
// Fishhawk's code-execution threat model — must not be anonymously readable.
// Operator tokens carry it via operatorDefaultScopes; cookie-session operators
// bypass scope enforcement (requireWriteScope's contract); an mcp:run token is
// authorized instead by the cross-run subject guard below.
const scopeGateViewRead = "read:audit"

// handleGetRunGateView implements GET /v0/runs/{run_id}/gate-view (#1960).
//
// Auth mirrors handleListRunAudit's read posture: a read scope for ordinary
// callers PLUS the fixup handler's mcp:run cross-run subject guard.
func (s *Server) handleGetRunGateView(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ConcernRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "gate_view_unconfigured",
			"gate-view endpoint requires a configured concern repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	// Optional stage_kind filter — plan|implement only. Validated before any
	// DB read so a malformed value fails cheap.
	stageKind := r.URL.Query().Get("stage_kind")
	if stageKind != "" && stageKind != concern.StageKindPlan && stageKind != concern.StageKindImplement {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_kind must be 'plan' or 'implement' when set",
			map[string]any{"field": "stage_kind", "got": stageKind})
		return
	}

	// Read authorization. An mcp:run:<uuid> run-bound token is authorized by
	// the cross-run subject guard alone — it is inherently scoped to its own
	// run (and structurally cannot carry the read scope), mirroring
	// handleFixupStage's cross_run_fixup guard. Every OTHER caller (operator
	// token, cookie session, anonymous) must clear the read scope:
	// requireWriteScope 401s an anonymous caller, 403s a token missing the
	// scope, and bypasses cookie-session operators. Full reviewer prose must
	// not be anonymously readable (#1960 authz).
	id := IdentityFrom(r.Context())
	if strings.HasPrefix(id.Subject, "mcp:run:") {
		subjectRunID, parseErr := uuid.Parse(strings.TrimPrefix(id.Subject, "mcp:run:"))
		if parseErr != nil {
			s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
				"mcp token subject is malformed", nil)
			return
		}
		if subjectRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "cross_run_gate_view",
				"mcp token may only read the gate view of its own run",
				map[string]any{
					"token_run_id": subjectRunID.String(),
					"path_run_id":  runID.String(),
				})
			return
		}
	} else if !s.requireWriteScope(w, r, scopeGateViewRead) {
		return
	}

	// Authoritative existence check: an unknown run is a 404, distinct from a
	// known run with zero concerns (which ListByRun cannot distinguish).
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"gate-view endpoint requires a configured run repository", nil)
		return
	}
	if _, err := s.cfg.RunRepo.GetRun(r.Context(), runID); err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	rows, err := s.cfg.ConcernRepo.ListByRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list concerns failed", map[string]any{"error": err.Error()})
		return
	}

	resp := s.buildGateView(r.Context(), runID, stageKind, rows)
	s.writeJSON(w, r, http.StatusOK, resp)
}

// buildGateView assembles the gate-view response from the run's concern rows
// plus the audit-reconstructed history. It never fails: an unreadable audit
// history degrades visibly (HistoryIncomplete + HistoryGaps) with the concerns
// intact.
func (s *Server) buildGateView(ctx context.Context, runID uuid.UUID, stageKind string, rows []*concern.Concern) gateViewResponse {
	resp := gateViewResponse{
		RunID:                   runID,
		StageKind:               stageKind,
		Open:                    []gateViewConcern{},
		Settled:                 []gateViewSettledConcern{},
		SuppressedRelitigations: []gateViewSuppressedRelitig{},
	}

	// Fetch the audit history, category by category, so a single-category read
	// failure names that gap while the rest of the joins still build.
	history := s.loadGateViewHistory(ctx, runID, &resp)

	for _, c := range rows {
		if stageKind != "" && c.StageKind != stageKind {
			continue
		}
		if c.State.IsOpen() {
			resp.Open = append(resp.Open, gateViewOpenConcern(c, history))
			continue
		}
		resp.Settled = append(resp.Settled, gateViewSettledConcern{
			ID:            c.ID,
			StageKind:     c.StageKind,
			State:         string(c.State),
			Severity:      c.Severity,
			Category:      c.Category,
			ReviewerModel: derefStr(c.ReviewerModel),
			Note:          c.Note,
			StateReason:   c.StateReason,
		})
	}

	resp.SuppressedRelitigations = history.suppressed
	return resp
}

// gateViewHistory holds the sorted, decoded audit joins the concern loop reads.
type gateViewHistory struct {
	triggers    []gateViewTrigger
	outcomes    []gateViewOutcome
	resolutions []gateViewReviewResolution
	suppressed  []gateViewSuppressedRelitig
}

type gateViewTrigger struct {
	sequence   int64
	stageID    *uuid.UUID
	concernIDs []string
	reason     string
}

type gateViewOutcome struct {
	sequence  int64
	stageID   *uuid.UUID
	outcome   string // "pushed" | "no_changes"
	applyPath string
	headSHA   string
}

type gateViewReviewResolution struct {
	sequence   int64
	implement  bool // true for implement_reviewed (round derivable), false for plan_reviewed
	concernID  string
	resolution string
	note       string
}

// loadGateViewHistory reads and decodes the audit categories the joins need.
// AuditRepo nil, or a per-category read error, sets HistoryIncomplete and
// appends the gap — the concerns are still returned. A malformed payload in a
// single entry is skipped warn-only while its siblings still decode.
func (s *Server) loadGateViewHistory(ctx context.Context, runID uuid.UUID, resp *gateViewResponse) gateViewHistory {
	var h gateViewHistory
	if s.cfg.AuditRepo == nil {
		resp.HistoryIncomplete = true
		resp.HistoryGaps = append(resp.HistoryGaps, gateViewHistoryCategories...)
		return h
	}

	byCategory := make(map[string][]*audit.Entry, len(gateViewHistoryCategories))
	for _, cat := range gateViewHistoryCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			resp.HistoryIncomplete = true
			resp.HistoryGaps = append(resp.HistoryGaps, cat)
			s.cfg.Logger.Warn("gate-view: list audit by category failed; history gap recorded",
				"run_id", runID.String(), "category", cat, "error", err.Error())
			continue
		}
		// Defensive sort: the join derives rounds and earliest-following
		// outcomes by Sequence and must not depend on the repo's return order.
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
		byCategory[cat] = entries
	}

	for _, e := range byCategory[CategoryStageFixupTriggered] {
		var p gateViewFixupTriggerPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			s.warnGateViewPayload(runID, CategoryStageFixupTriggered, e.Sequence, err)
			continue
		}
		h.triggers = append(h.triggers, gateViewTrigger{
			sequence:   e.Sequence,
			stageID:    e.StageID,
			concernIDs: p.ConcernIDs,
			reason:     p.Reason,
		})
	}
	for _, cat := range []string{"fixup_pushed", "fixup_no_changes"} {
		outcome := "pushed"
		if cat == "fixup_no_changes" {
			outcome = "no_changes"
		}
		for _, e := range byCategory[cat] {
			var p gateViewFixupOutcomePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnGateViewPayload(runID, cat, e.Sequence, err)
				continue
			}
			h.outcomes = append(h.outcomes, gateViewOutcome{
				sequence:  e.Sequence,
				stageID:   e.StageID,
				outcome:   outcome,
				applyPath: p.ApplyPath,
				headSHA:   p.HeadSHA,
			})
		}
	}
	for _, cat := range []string{"implement_reviewed", "plan_reviewed"} {
		implement := cat == "implement_reviewed"
		for _, e := range byCategory[cat] {
			var p gateViewResolutionsPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				s.warnGateViewPayload(runID, cat, e.Sequence, err)
				continue
			}
			for _, res := range p.ConcernResolutions {
				h.resolutions = append(h.resolutions, gateViewReviewResolution{
					sequence:   e.Sequence,
					implement:  implement,
					concernID:  res.ID,
					resolution: res.Resolution,
					note:       res.Note,
				})
			}
		}
	}
	for _, e := range byCategory[concernRelitigationSuppressedCategory] {
		var p concernRelitigationSuppressedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			s.warnGateViewPayload(runID, concernRelitigationSuppressedCategory, e.Sequence, err)
			continue
		}
		h.suppressed = append(h.suppressed, gateViewSuppressedRelitig(p))
	}
	return h
}

func (s *Server) warnGateViewPayload(runID uuid.UUID, category string, seq int64, err error) {
	s.cfg.Logger.Warn("gate-view: skipping malformed audit payload",
		"run_id", runID.String(), "category", category, "sequence", seq, "error", err.Error())
}

// gateViewOpenConcern renders one open concern with its full note plus the
// per-concern fix-up and resolution joins.
func gateViewOpenConcern(c *concern.Concern, h gateViewHistory) gateViewConcern {
	out := gateViewConcern{
		ID:                   c.ID,
		StageKind:            c.StageKind,
		OriginReviewSequence: c.OriginReviewSequence,
		ReviewerModel:        derefStr(c.ReviewerModel),
		Severity:             c.Severity,
		Category:             c.Category,
		State:                string(c.State),
		StateReason:          c.StateReason,
		Note:                 c.Note,
		HasSuggestedPatch:    c.SuggestedPatch != "",
	}
	if c.StageKind == concern.StageKindImplement {
		out.Round = gateViewRound(h.triggers, c.StageID, c.OriginReviewSequence)
	}

	idStr := c.ID.String()
	for _, t := range h.triggers {
		if !gateViewHasID(t.concernIDs, idStr) {
			continue
		}
		fx := gateViewFixup{
			Sequence: t.sequence,
			Reason:   t.reason,
			Outcome:  "pending",
		}
		if o, ok := earliestOutcomeAfter(h.outcomes, t.stageID, t.sequence); ok {
			fx.Outcome = o.outcome
			fx.ApplyPath = o.applyPath
			fx.HeadSHA = o.headSHA
		}
		out.Fixups = append(out.Fixups, fx)
	}

	for _, res := range h.resolutions {
		if res.concernID != idStr {
			continue
		}
		gr := gateViewResolution{
			Sequence:   res.sequence,
			Resolution: res.resolution,
			Note:       res.note,
		}
		if res.implement {
			gr.Round = gateViewRound(h.triggers, c.StageID, res.sequence)
		}
		out.Resolutions = append(out.Resolutions, gr)
	}
	return out
}

// gateViewRound derives the round of a review at reviewSeq for a given stage:
// 1 + the count of same-stage stage_fixup_triggered entries strictly below
// reviewSeq (the latestRoundConcerns convention, review_action_hint.go).
func gateViewRound(triggers []gateViewTrigger, stageID uuid.UUID, reviewSeq int64) int {
	n := 0
	for _, t := range triggers {
		if t.stageID == nil || *t.stageID != stageID {
			continue
		}
		if t.sequence < reviewSeq {
			n++
		}
	}
	return n + 1
}

// earliestOutcomeAfter returns the earliest fixup_pushed / fixup_no_changes
// outcome for triggerStage strictly after triggerSeq — the outcome of that
// fix-up pass. false when none has landed yet (a pending pass).
func earliestOutcomeAfter(outcomes []gateViewOutcome, triggerStage *uuid.UUID, triggerSeq int64) (gateViewOutcome, bool) {
	var best gateViewOutcome
	found := false
	for _, o := range outcomes {
		if o.sequence <= triggerSeq {
			continue
		}
		if !sameStage(o.stageID, triggerStage) {
			continue
		}
		if !found || o.sequence < best.sequence {
			best = o
			found = true
		}
	}
	return best, found
}

// sameStage reports whether two optional stage ids match. Two nil ids are
// treated as matching so a legacy entry without a stage id still joins.
func sameStage(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func gateViewHasID(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
