package server

// campaign_pending_amendments.go is the campaign-wide pending scope-amendment
// read (E86.85 / #4486): GET /v0/runs/{run_id}/campaign-pending-amendments and
// the pending_amendments block on GET /v0/campaigns/{campaign_id}/status.
//
// A driver blocked in an await verb on ONE run of a campaign could not see a
// scope amendment filed by a DIFFERENT run of the same campaign (the
// #4082/#4077 miss): the agent's ~15-minute poll window lapsed undecided. This
// read resolves the anchor run's campaign through the ancestry walk
// (resolveCampaignAncestry), enumerates every run of that campaign — each
// item's linked run, WHATEVER the item state (a cancelled or out-of-band
// succeeded item can still have a live run), plus its parent_run_id /
// decomposed_from descendants — and returns every PENDING amendment on them,
// oldest first. decision_live says whether the filing stage has not settled,
// so an agent may still be waiting on the decision; the MCP await verbs
// release only on a live entry.
//
// The payload names other runs' paths and reasons, so both surfaces refuse a
// run-bound agent token and require read:audit, mirroring the cross-run
// scope-overlap read.

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// campaignPendingAmendmentsMaxRuns bounds the run set one read enumerates.
// Reaching it stops enumeration and sets truncated; it is never silent.
const campaignPendingAmendmentsMaxRuns = 64

// campaignPendingAmendmentPayload is one pending amendment on a run of the
// campaign. Amendment is the existing list-item shape, so the #983 headroom
// rides along.
type campaignPendingAmendmentPayload struct {
	// IssueRef is the campaign item the filing run belongs to.
	IssueRef string `json:"issue_ref"`
	// ItemRunID is that campaign item's linked run (the filing run itself, or
	// the ancestor a slice / recovery child descends from).
	ItemRunID string `json:"item_run_id"`
	// StageState is the filing stage's current state.
	StageState string `json:"stage_state"`
	// DecisionLive is true while the filing stage has not settled, so an agent
	// may still be waiting on the decision.
	DecisionLive bool                   `json:"decision_live"`
	Amendment    scopeAmendmentResponse `json:"amendment"`
}

// campaignPendingAmendmentsPayload is the campaign-wide block: Items is ALWAYS
// an array (never null), oldest requested_at first.
type campaignPendingAmendmentsPayload struct {
	Items     []campaignPendingAmendmentPayload `json:"items"`
	Truncated bool                              `json:"truncated"`
}

// runCampaignPendingAmendmentsResponse is the GET
// /v0/runs/{run_id}/campaign-pending-amendments body. CampaignID is JSON null
// (not omitted) when the run is in no campaign.
type runCampaignPendingAmendmentsResponse struct {
	RunID      string                            `json:"run_id"`
	CampaignID *string                           `json:"campaign_id"`
	Items      []campaignPendingAmendmentPayload `json:"items"`
	Truncated  bool                              `json:"truncated"`
}

// campaignRunNode is one enumerated run with the campaign item it descends
// from.
type campaignRunNode struct {
	id    uuid.UUID
	depth int
	item  *campaign.Item
}

// campaignPendingAmendments enumerates every run of the campaign whose items
// are given and returns every pending amendment on them, oldest first. Every
// item's linked run seeds the walk whatever the item state; descendants are
// found breadth-first through BOTH the parent_run_id and decomposed_from links
// (unioned, so a recovery child carrying only parent_run_id and a slice are
// both found), depth-bounded by maxCampaignAncestorDepth and count-bounded by
// campaignPendingAmendmentsMaxRuns. ANY read error returns the error: there is
// no partial result.
func (s *Server) campaignPendingAmendments(ctx context.Context, items []*campaign.Item) (campaignPendingAmendmentsPayload, error) {
	out := campaignPendingAmendmentsPayload{Items: []campaignPendingAmendmentPayload{}}
	seen := map[uuid.UUID]bool{}
	var nodes []campaignRunNode
	add := func(n campaignRunNode) bool {
		if seen[n.id] {
			return true
		}
		if len(nodes) >= campaignPendingAmendmentsMaxRuns {
			out.Truncated = true
			return false
		}
		seen[n.id] = true
		nodes = append(nodes, n)
		return true
	}
	for _, it := range items {
		if it.RunID == nil {
			continue
		}
		if !add(campaignRunNode{id: *it.RunID, item: it}) {
			break
		}
	}
	for i := 0; i < len(nodes) && !out.Truncated; i++ {
		n := nodes[i]
		if n.depth >= maxCampaignAncestorDepth {
			continue
		}
		children, err := s.campaignRunChildren(ctx, n.id)
		if err != nil {
			return campaignPendingAmendmentsPayload{}, err
		}
		for _, c := range children {
			if !add(campaignRunNode{id: c, depth: n.depth + 1, item: n.item}) {
				break
			}
		}
	}
	for _, n := range nodes {
		rows, _, err := s.buildScopeAmendmentItems(ctx, n.id)
		if err != nil {
			return campaignPendingAmendmentsPayload{}, err
		}
		for _, row := range rows {
			if row.Status != string(scopeamendment.StatusPending) {
				continue
			}
			stage, err := s.cfg.RunRepo.GetStage(ctx, row.StageID)
			if err != nil {
				return campaignPendingAmendmentsPayload{}, err
			}
			entry := campaignPendingAmendmentPayload{
				IssueRef:  n.item.IssueRef,
				ItemRunID: n.item.RunID.String(),
				Amendment: row,
			}
			entry.StageState = string(stage.State)
			entry.DecisionLive = !run.StageState(stage.State).IsSettled()
			out.Items = append(out.Items, entry)
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		a, b := out.Items[i].Amendment, out.Items[j].Amendment
		if !a.RequestedAt.Equal(b.RequestedAt) {
			return a.RequestedAt.Before(b.RequestedAt)
		}
		return a.ID.String() < b.ID.String()
	})
	return out, nil
}

// campaignRunChildren lists the direct children of runID through both lineage
// links, de-duplicated in listing order. Each ListRuns call carries an
// explicit Limit (the Postgres adapter refuses Limit <= 0); one above the run
// cap keeps truncated derivable from a single over-full listing.
func (s *Server) campaignRunChildren(ctx context.Context, runID uuid.UUID) ([]uuid.UUID, error) {
	id := runID
	byParent, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{ParentRunID: &id, Limit: campaignPendingAmendmentsMaxRuns + 1})
	if err != nil {
		return nil, err
	}
	byDecomposition, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{DecomposedFrom: &id, Limit: campaignPendingAmendmentsMaxRuns + 1})
	if err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	out := make([]uuid.UUID, 0, len(byParent)+len(byDecomposition))
	for _, r := range append(byParent, byDecomposition...) {
		if r.ID == runID || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		out = append(out, r.ID)
	}
	return out, nil
}

// campaignAmendmentsReadable reports whether id may read the campaign status
// pending_amendments block. A run-bound agent token never reads other runs'
// amendment paths and reasons; an anonymous caller is refused; a token caller
// needs read:audit; a cookie-session operator passes — the same outcome
// requireWriteScope gives the route.
func campaignAmendmentsReadable(id Identity) bool {
	if isRunBoundSubject(id.Subject) {
		return false
	}
	if id.IsAnonymous() {
		return false
	}
	if id.TokenID != "" {
		return hasScope(id, scopeGateViewRead)
	}
	return true
}

// campaignPendingAmendmentsForStatus is the status read's pending_amendments
// block: nil (key omitted) when the repositories are unwired, the caller may
// not read it, or the enumeration failed (WARN-logged; the status read never
// fails on it). An absent key means NOT COMPUTED, which is not the same claim
// as a present block with items [] (computed, none pending).
func (s *Server) campaignPendingAmendmentsForStatus(ctx context.Context, c *campaign.Campaign, items []*campaign.Item) *campaignPendingAmendmentsPayload {
	if s.cfg.ScopeAmendmentRepo == nil || s.cfg.RunRepo == nil || !campaignAmendmentsReadable(IdentityFrom(ctx)) {
		return nil
	}
	payload, err := s.campaignPendingAmendments(ctx, items)
	if err != nil {
		s.cfg.Logger.Warn("campaign status: pending amendments read failed; omitting pending_amendments",
			"campaign_id", c.ID.String(), "error", err.Error())
		return nil
	}
	return &payload
}

// handleGetRunCampaignPendingAmendments implements GET
// /v0/runs/{run_id}/campaign-pending-amendments.
//
// Read authorization: the payload names OTHER runs' amendment paths and
// reasons, so a run-bound mcp:run:<uuid> token is refused 403
// run_token_forbidden even for its own run. Every other caller must clear
// scopeGateViewRead (read:audit) via requireWriteScope: 401 anonymous, 403 a
// token missing the scope; a cookie-session operator passes.
func (s *Server) handleGetRunCampaignPendingAmendments(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"campaign pending-amendments endpoint requires a configured run repository", nil)
		return
	}
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaign pending-amendments endpoint requires a configured campaign repository", nil)
		return
	}
	if s.cfg.ScopeAmendmentRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "scope_amendment_unconfigured",
			"campaign pending-amendments endpoint requires a configured scope-amendment repository", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	if isRunBoundSubject(IdentityFrom(r.Context()).Subject) {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not read campaign-wide scope amendments; they name other runs' paths and reasons", nil)
		return
	}
	if !s.requireWriteScope(w, r, scopeGateViewRead) {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}
	resp := runCampaignPendingAmendmentsResponse{RunID: runID.String(), Items: []campaignPendingAmendmentPayload{}}
	anc, err := s.resolveCampaignAncestry(r.Context(), runRow)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"resolve the run's campaign membership failed", map[string]any{"error": err.Error()})
		return
	}
	if anc.unresolved != "" || len(anc.items) == 0 {
		s.writeJSON(w, r, http.StatusOK, resp)
		return
	}
	campaignID := anc.items[0].CampaignID
	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), campaignID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}
	payload, err := s.campaignPendingAmendments(r.Context(), items)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign pending amendments failed", map[string]any{"error": err.Error()})
		return
	}
	cid := campaignID.String()
	resp.CampaignID = &cid
	resp.Items = payload.Items
	resp.Truncated = payload.Truncated
	s.writeJSON(w, r, http.StatusOK, resp)
}
