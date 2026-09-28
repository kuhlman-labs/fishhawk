package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// GET /v0/attention (E40.1 / #1713) is the cross-run "Needs You" queue: one
// prioritized, account- and repo-narrowed list of every decision currently
// parked on a human. It is a pure PROJECTION of already-recorded state — it
// writes nothing, mints no audit entry, and reuses the existing derivations
// (acceptanceGateState, buildGateView, campaign.NextEligible) rather than
// re-deriving any gate rule. Long-form contract: backend/internal/server/README.md
// § "Attention queue".
//
// Every read beyond the run scan is INDEPENDENTLY degradable: a nil store or a
// per-run read error appends a named reason to degraded[] and drops only that
// contribution. Only the run repository is required. Completeness is never
// silent — both bounded scans (runs, campaigns) and the gate-view budget
// report when they bite.

const (
	// attentionRunScanLimit bounds the candidate non-terminal runs scanned per
	// request (pending + running combined). A bite sets truncated=true.
	attentionRunScanLimit = 100
	// attentionGateViewBudget bounds the per-request buildGateView calls (each
	// is several audit reads). Runs past the budget contribute no concern items
	// and a gate_view_budget_exhausted degraded entry names how many.
	attentionGateViewBudget = 25
	// attentionCampaignScanLimit bounds the awaiting_human campaigns scanned. A
	// bite appends the campaign_scan_truncated degraded reason.
	attentionCampaignScanLimit = 50

	attentionDefaultLimit = 100
	attentionMaxLimit     = 500
)

// Attention item kinds — a closed six-member set mirrored by the OpenAPI
// AttentionItem.kind enum.
const (
	attentionKindPlanGate              = "plan_gate"
	attentionKindScopeAmendment        = "scope_amendment"
	attentionKindAcceptanceDisposition = "acceptance_disposition"
	attentionKindSplitVerdict          = "split_verdict"
	attentionKindPagedConcern          = "paged_concern"
	attentionKindAttendHumanLed        = "attend_human_led_campaign"
)

// attentionPriority is the fixed rank table (1 = most urgent). Items sort by
// (priority ASC, since ASC, id ASC), so the order is total and stable.
var attentionPriority = map[string]int{
	attentionKindPlanGate:              1,
	attentionKindScopeAmendment:        2,
	attentionKindAcceptanceDisposition: 3,
	attentionKindSplitVerdict:          4,
	attentionKindPagedConcern:          5,
	attentionKindAttendHumanLed:        6,
}

// Degraded reasons — machine-readable names for a contribution the response
// could not include. Mirrored by the OpenAPI AttentionDegraded.reason enum.
const (
	attentionDegradedConcernStoreUnconfigured   = "concern_store_unconfigured"
	attentionDegradedAmendmentStoreUnconfigured = "scope_amendment_store_unconfigured"
	attentionDegradedCampaignStoreUnconfigured  = "campaign_store_unconfigured"
	attentionDegradedStageReadFailed            = "stage_read_failed"
	attentionDegradedAcceptanceUnreadable       = "acceptance_state_unreadable"
	attentionDegradedAmendmentReadFailed        = "scope_amendment_read_failed"
	attentionDegradedConcernReadFailed          = "concern_read_failed"
	attentionDegradedGateViewHistoryIncomplete  = "gate_view_history_incomplete"
	attentionDegradedGateViewBudgetExhausted    = "gate_view_budget_exhausted"
	attentionDegradedPlanSummaryUnavailable     = "plan_summary_unavailable"
	attentionDegradedPlanReviewsUnreadable      = "plan_reviews_unreadable"
	attentionDegradedCampaignReadFailed         = "campaign_read_failed"
	attentionDegradedCampaignScanTruncated      = "campaign_scan_truncated"
	attentionDegradedCampaignItemsUnreadable    = "campaign_items_unreadable"
)

// attentionResponse is the GET /v0/attention body.
type attentionResponse struct {
	Items    []attentionItem     `json:"items"`
	Degraded []attentionDegraded `json:"degraded"`
	// Truncated is true when the candidate-run scan hit attentionRunScanLimit
	// or the item list was cut at ?limit. Campaign-scan truncation is reported
	// as a campaign_scan_truncated degraded entry instead.
	Truncated   bool `json:"truncated"`
	ScannedRuns int  `json:"scanned_runs"`
}

// attentionDegraded names one contribution the response could not include.
type attentionDegraded struct {
	Reason     string `json:"reason"`
	RunID      string `json:"run_id,omitempty"`
	CampaignID string `json:"campaign_id,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// attentionItem is one parked decision.
type attentionItem struct {
	// ID is the stable identity of the thing the decision is about (the stage,
	// amendment, concern or campaign id) — the final sort tiebreak and a
	// client key.
	ID          string           `json:"id"`
	Kind        string           `json:"kind"`
	Priority    int              `json:"priority"`
	RunID       string           `json:"run_id,omitempty"`
	CampaignID  string           `json:"campaign_id,omitempty"`
	StageID     string           `json:"stage_id,omitempty"`
	ConcernID   string           `json:"concern_id,omitempty"`
	AmendmentID string           `json:"amendment_id,omitempty"`
	Repo        string           `json:"repo"`
	Title       string           `json:"title"`
	Context     attentionContext `json:"context"`
	// DetailPath is the SPA-relative link target: /runs/{id},
	// /runs/{id}/stages/{id} or /campaigns/{id}. Emitted server-side so the six
	// link rules live in one place.
	DetailPath string    `json:"detail_path"`
	Since      time.Time `json:"since"`
}

// attentionContext is the one-screen decision context. Which fields are set
// depends on the item kind (see the OpenAPI AttentionContext schema).
type attentionContext struct {
	// plan_gate
	PlanSummary    string                   `json:"plan_summary,omitempty"`
	ReviewVerdicts []attentionReviewVerdict `json:"review_verdicts,omitempty"`
	// scope_amendment
	Reason         string                     `json:"reason,omitempty"`
	RequestedPaths []scopeamendment.PathEntry `json:"requested_paths,omitempty"`
	// acceptance_disposition
	Verdict         string `json:"verdict,omitempty"`
	CriteriaFailed  *int   `json:"criteria_failed,omitempty"`
	CriteriaSkipped *int   `json:"criteria_skipped,omitempty"`
	// FailedCriteria names WHICH criteria failed plus each one's
	// decision-relevant explanation (the request whose response the failing
	// assertion evaluated), so the acceptance card conveys the disposition
	// without a detail-page fetch. Empty when the outcome carries no agreeing
	// transcript (a legacy verdict, or a transcript suppressed for disagreeing
	// with the verdict rows).
	FailedCriteria []attentionFailedCriterion `json:"failed_criteria,omitempty"`
	// split_verdict / paged_concern
	StageKind      string   `json:"stage_kind,omitempty"`
	Severity       string   `json:"severity,omitempty"`
	Category       string   `json:"category,omitempty"`
	ReviewerModel  string   `json:"reviewer_model,omitempty"`
	Note           string   `json:"note,omitempty"`
	NewEvidence    string   `json:"new_evidence,omitempty"`
	DisputeReasons []string `json:"dispute_reasons,omitempty"`
	// ConfirmationNote is a split verdict's other side: the note of the newest
	// re-review that recorded the still-open concern as confirmed resolved.
	ConfirmationNote string `json:"confirmation_note,omitempty"`
	// attend_human_led_campaign
	EpicRef      string   `json:"epic_ref,omitempty"`
	HumanLedRefs []string `json:"human_led_refs,omitempty"`
	// Detail is shared prose: the campaign next-action detail.
	Detail string `json:"detail,omitempty"`
}

// attentionReviewVerdict is one recorded plan-review verdict.
type attentionReviewVerdict struct {
	ReviewerModel string `json:"reviewer_model,omitempty"`
	Verdict       string `json:"verdict"`
	ConcernCount  int    `json:"concern_count"`
}

// attentionFailedCriterion is one failed acceptance criterion: its id and the
// request whose response the failing assertion evaluated (method, path,
// status) — the decision-relevant explanation carried inline on the card. The
// request fields are empty when the failed criterion recorded no requests
// (FailingRequest was null in the transcript summary).
type attentionFailedCriterion struct {
	ID     string `json:"id"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Status int    `json:"status,omitempty"`
}

// attentionCollector accumulates items and degraded reasons for one request.
type attentionCollector struct {
	items    []attentionItem
	degraded []attentionDegraded
}

func (c *attentionCollector) degrade(reason, runID, campaignID, detail string) {
	c.degraded = append(c.degraded, attentionDegraded{Reason: reason, RunID: runID, CampaignID: campaignID, Detail: detail})
}

func (c *attentionCollector) add(it attentionItem) {
	it.Priority = attentionPriority[it.Kind]
	c.items = append(c.items, it)
}

// handleListAttention implements GET /v0/attention.
//
// Auth: the items carry FULL reviewer concern prose (as the gate view does),
// so the read requires the same read scope as GET /v0/runs/{id}/gate-view
// (scopeGateViewRead); cookie-session operators bypass scope enforcement per
// requireWriteScope's contract. Narrowing mirrors handleListRuns exactly: the
// caller's Identity.AccountID bounds the run and campaign scans, and the
// repo-visibility filter drops rows the caller cannot read — a filter fault
// fails the WHOLE request CLOSED (503), never a partially-narrowed page.
func (s *Server) handleListAttention(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeGateViewRead) {
		return
	}
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"attention endpoint requires a configured run repository", nil)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"), attentionDefaultLimit, attentionMaxLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return
	}
	ctx := r.Context()
	accountFilter := IdentityFrom(ctx).AccountID

	// ListRunsFilter.State is a single string, so the non-terminal candidate
	// set costs two scans. Each asks for one row past the cap so a bite is
	// observable without a COUNT.
	var candidates []*run.Run
	for _, st := range []run.State{run.StatePending, run.StateRunning} {
		rows, lerr := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			State:     string(st),
			AccountID: accountFilter,
			Limit:     attentionRunScanLimit + 1,
		})
		if lerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list runs failed", map[string]any{"error": lerr.Error()})
			return
		}
		candidates = append(candidates, rows...)
	}
	// Oldest first: the longest-parked runs win the bounded scan.
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID.String() < candidates[j].ID.String()
	})
	resp := attentionResponse{}
	if len(candidates) > attentionRunScanLimit {
		candidates = candidates[:attentionRunScanLimit]
		resp.Truncated = true
	}

	filter, ok := s.requestRepoFilter(w, r)
	if !ok {
		return
	}
	var visible []*run.Run
	for _, ru := range candidates {
		allowed, ferr := filter.allows(ctx, ru.Repo)
		if ferr != nil {
			s.writeRepoFilterUnavailable(w, r)
			return
		}
		if !allowed {
			continue
		}
		visible = append(visible, ru)
	}
	resp.ScannedRuns = len(visible)

	col := &attentionCollector{}
	if s.cfg.ConcernRepo == nil {
		col.degrade(attentionDegradedConcernStoreUnconfigured, "", "", "")
	}
	if s.cfg.ScopeAmendmentRepo == nil {
		col.degrade(attentionDegradedAmendmentStoreUnconfigured, "", "", "")
	}
	gateViews, overBudget := 0, 0
	for _, ru := range visible {
		s.collectRunAttention(ctx, ru, col, &gateViews, &overBudget)
	}
	if overBudget > 0 {
		col.degrade(attentionDegradedGateViewBudgetExhausted, "", "",
			fmt.Sprintf("%d run(s) with open concerns past the %d-run gate-view budget contributed no concern items", overBudget, attentionGateViewBudget))
	}

	// The campaign scan applies the SAME fail-closed repo filter: a filter
	// fault is a 503 here exactly as for runs.
	if !s.collectCampaignAttention(ctx, w, r, filter, accountFilter, col) {
		return
	}

	sort.SliceStable(col.items, func(i, j int) bool {
		a, b := col.items[i], col.items[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.ID < b.ID
	})
	if len(col.items) > limit {
		col.items = col.items[:limit]
		resp.Truncated = true
	}
	resp.Items = col.items
	if resp.Items == nil {
		resp.Items = []attentionItem{}
	}
	resp.Degraded = col.degraded
	if resp.Degraded == nil {
		resp.Degraded = []attentionDegraded{}
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// collectRunAttention derives every item one run contributes. Each derivation
// is independent: a read failure degrades only its own contribution.
func (s *Server) collectRunAttention(ctx context.Context, ru *run.Run, col *attentionCollector, gateViews, overBudget *int) {
	runID := ru.ID.String()
	title := attentionRunTitle(ru)
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, ru.ID)
	if err != nil {
		// Without stages no gate derivation is possible; the amendment read
		// below does not need them, but a run whose stages are unreadable is
		// reported once and skipped whole so the page never half-describes it.
		col.degrade(attentionDegradedStageReadFailed, runID, "", err.Error())
		return
	}

	var reviewGate *run.Stage
	for _, st := range stages {
		if st.State != run.StageStateAwaitingApproval {
			continue
		}
		switch st.Type {
		case run.StageTypePlan:
			col.add(s.planGateItem(ctx, ru, st, title, col))
		case run.StageTypeImplement, run.StageTypeReview:
			if reviewGate == nil {
				reviewGate = st
			}
		}
	}

	// (b) acceptance disposition parked at triage.
	gate, aerr := s.acceptanceGateState(ctx, ru, stages)
	if aerr != nil {
		col.degrade(attentionDegradedAcceptanceUnreadable, runID, "", aerr.Error())
	} else if gate == acceptanceGateTriage {
		col.add(s.acceptanceItem(ctx, ru, stages, title, col))
	}

	// (c) pending scope amendments.
	if s.cfg.ScopeAmendmentRepo != nil {
		rows, lerr := s.cfg.ScopeAmendmentRepo.ListByRun(ctx, ru.ID)
		if lerr != nil {
			col.degrade(attentionDegradedAmendmentReadFailed, runID, "", lerr.Error())
		}
		for _, a := range rows {
			if a.Status != scopeamendment.StatusPending {
				continue
			}
			col.add(attentionItem{
				ID:          a.ID.String(),
				Kind:        attentionKindScopeAmendment,
				RunID:       runID,
				StageID:     a.StageID.String(),
				AmendmentID: a.ID.String(),
				Repo:        ru.Repo,
				Title:       title,
				Context:     attentionContext{Reason: a.Reason, RequestedPaths: a.Paths},
				DetailPath:  "/runs/" + runID + "/stages/" + a.StageID.String(),
				Since:       a.RequestedAt,
			})
		}
	}

	// (d) open concerns at a parked review gate: split verdicts + paged concerns.
	if s.cfg.ConcernRepo == nil || reviewGate == nil {
		return
	}
	open, cerr := s.cfg.ConcernRepo.ListOpenByRun(ctx, ru.ID)
	if cerr != nil {
		col.degrade(attentionDegradedConcernReadFailed, runID, "", cerr.Error())
		return
	}
	if len(open) == 0 {
		return
	}
	if *gateViews >= attentionGateViewBudget {
		*overBudget++
		return
	}
	*gateViews++
	gv := s.buildGateView(ctx, ru.ID, "", open)
	if gv.HistoryIncomplete {
		col.degrade(attentionDegradedGateViewHistoryIncomplete, runID, "", strings.Join(gv.HistoryGaps, ","))
	}
	since := map[uuid.UUID]time.Time{}
	for _, c := range open {
		since[c.ID] = c.CreatedAt
	}
	for _, c := range gv.Open {
		kind := attentionKindPagedConcern
		var reasons []string
		var confirmation string
		if c.Disputed {
			kind = attentionKindSplitVerdict
			for _, d := range c.Disputes {
				reasons = append(reasons, d.VetoReason)
			}
			// The other side of the split: the newest recorded confirmation.
			for _, res := range c.Resolutions {
				if res.Resolution == "confirmed" {
					confirmation = res.Note
				}
			}
		}
		col.add(attentionItem{
			ID:         c.ID.String(),
			Kind:       kind,
			RunID:      runID,
			StageID:    reviewGate.ID.String(),
			ConcernID:  c.ID.String(),
			Repo:       ru.Repo,
			Title:      title,
			DetailPath: "/runs/" + runID + "/stages/" + reviewGate.ID.String(),
			Since:      since[c.ID],
			Context: attentionContext{
				StageKind:        c.StageKind,
				Severity:         c.Severity,
				Category:         c.Category,
				ReviewerModel:    c.ReviewerModel,
				Note:             c.Note,
				NewEvidence:      c.NewEvidence,
				DisputeReasons:   reasons,
				ConfirmationNote: confirmation,
			},
		})
	}
}

// planGateItem builds the plan_gate item: the plan's summary line plus the
// recorded plan-review verdicts. Both reads degrade visibly and never drop the
// item — a parked plan gate is shown even when its context is unreadable.
func (s *Server) planGateItem(ctx context.Context, ru *run.Run, st *run.Stage, title string, col *attentionCollector) attentionItem {
	runID := ru.ID.String()
	it := attentionItem{
		ID:         st.ID.String(),
		Kind:       attentionKindPlanGate,
		RunID:      runID,
		StageID:    st.ID.String(),
		Repo:       ru.Repo,
		Title:      title,
		DetailPath: "/runs/" + runID + "/stages/" + st.ID.String(),
		Since:      attentionStageSince(st, ru),
	}
	summary, err := s.attentionPlanSummary(ctx, st.ID)
	if err != nil {
		col.degrade(attentionDegradedPlanSummaryUnavailable, runID, "", err.Error())
	}
	it.Context.PlanSummary = summary
	if s.cfg.AuditRepo == nil {
		// No audit store → the plan-review verdicts cannot be read. Report the
		// thinned contribution rather than dropping it silently; the item is
		// still shown with its summary.
		col.degrade(attentionDegradedPlanReviewsUnreadable, runID, "", "audit store unconfigured")
	} else {
		entries, aerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, ru.ID, "plan_reviewed")
		if aerr != nil {
			col.degrade(attentionDegradedPlanReviewsUnreadable, runID, "", aerr.Error())
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
		for _, e := range entries {
			var p struct {
				ReviewerModel string            `json:"reviewer_model"`
				Verdict       string            `json:"verdict"`
				Concerns      []json.RawMessage `json:"concerns"`
			}
			if json.Unmarshal(e.Payload, &p) != nil || p.Verdict == "" {
				continue
			}
			it.Context.ReviewVerdicts = append(it.Context.ReviewVerdicts, attentionReviewVerdict{
				ReviewerModel: p.ReviewerModel, Verdict: p.Verdict, ConcernCount: len(p.Concerns),
			})
		}
	}
	return it
}

// attentionPlanSummary reads the plan stage's newest standard_v1 plan artifact
// summary. An unconfigured artifact store is reported as an error so the
// caller's degraded entry names it.
func (s *Server) attentionPlanSummary(ctx context.Context, stageID uuid.UUID) (string, error) {
	if s.cfg.ArtifactRepo == nil {
		return "", fmt.Errorf("artifact store unconfigured")
	}
	arts, err := s.cfg.ArtifactRepo.ListForStage(ctx, stageID)
	if err != nil {
		return "", err
	}
	var picked *artifact.Artifact
	for _, a := range arts {
		if a.Kind != artifact.KindPlan || a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		if picked == nil || a.CreatedAt.After(picked.CreatedAt) {
			picked = a
		}
	}
	if picked == nil {
		return "", nil
	}
	var p plan.Plan
	if err := json.Unmarshal(picked.Content, &p); err != nil {
		return "", fmt.Errorf("plan artifact undecodable: %w", err)
	}
	return p.Summary, nil
}

// acceptanceItem builds the acceptance_disposition item from the newest
// recorded outcome (already known to be an unarbitrated failure). The outcome
// transcript supplies WHICH criteria failed and each one's decision-relevant
// explanation, carried inline as FailedCriteria.
func (s *Server) acceptanceItem(ctx context.Context, ru *run.Run, stages []*run.Stage, title string, col *attentionCollector) attentionItem {
	runID := ru.ID.String()
	it := attentionItem{
		Kind:       attentionKindAcceptanceDisposition,
		RunID:      runID,
		Repo:       ru.Repo,
		Title:      title,
		DetailPath: "/runs/" + runID,
		Since:      ru.UpdatedAt,
		ID:         runID,
	}
	if acc := acceptanceStageOf(stages); acc != nil {
		it.ID = acc.ID.String()
		it.StageID = acc.ID.String()
		it.DetailPath = "/runs/" + runID + "/stages/" + acc.ID.String()
		it.Since = attentionStageSince(acc, ru)
	}
	// acceptanceGateState just read this outcome successfully. A second read
	// failing here thins the context to nothing (no verdict, no criteria) —
	// which would silently contradict the one-screen-context contract — so it
	// is reported as acceptance_state_unreadable rather than swallowed. The
	// item is still emitted (the gate parked it; the operator must see it).
	out, err := s.latestAcceptanceOutcome(ctx, ru.ID)
	if err != nil {
		col.degrade(attentionDegradedAcceptanceUnreadable, runID, "",
			"acceptance outcome unreadable on re-read; card context thinned: "+err.Error())
		return it
	}
	if out.Recorded {
		failed, skipped := out.CriteriaFailed, out.CriteriaSkipped
		it.Context.Verdict = out.Verdict
		it.Context.CriteriaFailed = &failed
		it.Context.CriteriaSkipped = &skipped
		if out.Transcript != nil {
			for _, c := range out.Transcript.Criteria {
				if c.Outcome != "failed" {
					continue
				}
				fc := attentionFailedCriterion{ID: c.ID}
				if c.FailingRequest != nil {
					fc.Method = c.FailingRequest.Method
					fc.Path = c.FailingRequest.Path
					fc.Status = c.FailingRequest.Status
				}
				it.Context.FailedCriteria = append(it.Context.FailedCriteria, fc)
			}
		}
	}
	return it
}

// collectCampaignAttention appends one attend_human_led_campaign item per
// visible awaiting_human campaign. The campaign's persisted state is the
// durable signal; the human-led refs come from the item partition. Returns
// false when it already wrote the 503 repo-filter envelope.
func (s *Server) collectCampaignAttention(ctx context.Context, w http.ResponseWriter, r *http.Request, filter *repoFilter, accountFilter string, col *attentionCollector) bool {
	if s.cfg.CampaignRepo == nil {
		col.degrade(attentionDegradedCampaignStoreUnconfigured, "", "", "")
		return true
	}
	rows, err := s.cfg.CampaignRepo.ListCampaigns(ctx, campaign.ListCampaignsFilter{
		State:     string(campaign.StateAwaitingHuman),
		AccountID: accountFilter,
		Limit:     attentionCampaignScanLimit + 1,
	})
	if err != nil {
		col.degrade(attentionDegradedCampaignReadFailed, "", "", err.Error())
		return true
	}
	if len(rows) > attentionCampaignScanLimit {
		rows = rows[:attentionCampaignScanLimit]
		col.degrade(attentionDegradedCampaignScanTruncated, "", "",
			fmt.Sprintf("more than %d campaigns are awaiting a human; only the first %d are listed", attentionCampaignScanLimit, attentionCampaignScanLimit))
	}
	for _, c := range rows {
		allowed, ferr := filter.allows(ctx, c.Repo)
		if ferr != nil {
			s.writeRepoFilterUnavailable(w, r)
			return false
		}
		if !allowed {
			continue
		}
		cid := c.ID.String()
		it := attentionItem{
			ID:         cid,
			Kind:       attentionKindAttendHumanLed,
			CampaignID: cid,
			Repo:       c.Repo,
			Title:      attentionCampaignTitle(c),
			DetailPath: "/campaigns/" + cid,
			Since:      c.UpdatedAt,
			Context:    attentionContext{EpicRef: c.EpicRef},
		}
		items, ierr := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(ctx, c.ID)
		if ierr != nil {
			col.degrade(attentionDegradedCampaignItemsUnreadable, "", cid, ierr.Error())
		} else {
			elig := campaign.NextEligible(items)
			it.Context.HumanLedRefs = elig.HumanLed
			it.Context.Detail = computeCampaignNextAction(c.State, elig).Detail
		}
		col.add(it)
	}
	return true
}

// attentionStageSince is when a stage parked: its end if recorded, else its
// start or dispatch, else the run's last update.
func attentionStageSince(st *run.Stage, ru *run.Run) time.Time {
	for _, t := range []*time.Time{st.EndedAt, st.StartedAt, st.DispatchedAt} {
		if t != nil {
			return *t
		}
	}
	return ru.UpdatedAt
}

func attentionRunTitle(ru *run.Run) string {
	if ru.IssueContext != nil && ru.IssueContext.Title != "" {
		return ru.IssueContext.Title
	}
	if ru.TriggerRef != nil && *ru.TriggerRef != "" {
		return *ru.TriggerRef
	}
	return "run " + ru.ID.String()
}

func attentionCampaignTitle(c *campaign.Campaign) string {
	if c.EpicRef != "" {
		return "campaign " + c.EpicRef
	}
	return "campaign " + c.ID.String()
}
