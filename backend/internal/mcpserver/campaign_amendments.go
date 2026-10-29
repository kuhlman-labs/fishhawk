package mcpserver

// campaign_amendments.go is the MCP half of the campaign-wide pending
// scope-amendment release (E86.85 / #4486). A driver blocked in
// fishhawk_await_stage or fishhawk_await_children on ONE run of a campaign could
// not see a scope amendment filed by a DIFFERENT run of the same campaign (the
// #4082/#4077 miss): the filing agent's poll window lapsed undecided while the
// driver's wait held. Both await verbs now probe GET
// /v0/runs/{run_id}/campaign-pending-amendments after their own amendment arm
// finds nothing, and release with the EXISTING amendment_pending status on the
// oldest decision-live amendment anywhere else in the campaign.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// campaignPendingAmendment mirrors the backend's campaignPendingAmendmentPayload
// (server/campaign_pending_amendments.go): one PENDING amendment on a run of the
// campaign. Unexported, so the export baseline is unchanged.
type campaignPendingAmendment struct {
	// IssueRef is the campaign item the filing run belongs to.
	IssueRef string `json:"issue_ref" jsonschema:"the campaign item the filing run belongs to"`
	// ItemRunID is that item's linked run (the filing run itself, or the
	// ancestor a slice / recovery child descends from).
	ItemRunID string `json:"item_run_id" jsonschema:"the campaign item's linked run: the filing run itself, or the ancestor its decomposition slice or recovery child descends from"`
	// StageState is the filing stage's current state.
	StageState string `json:"stage_state" jsonschema:"the filing stage's current state"`
	// DecisionLive is true while the filing stage has not settled, so an agent
	// may still be waiting on the decision.
	DecisionLive bool `json:"decision_live" jsonschema:"true while the filing stage has not settled, so its agent may still be waiting on the decision; false for a still-pending request on a settled (e.g. failed) stage, which is listed but never releases a wait"`
	// Amendment is the full scope-amendment row (with the #983 headroom).
	Amendment ScopeAmendmentItem `json:"amendment" jsonschema:"the full pending scope-amendment row; decide it with fishhawk_decide_scope_amendment on amendment.run_id (the FILING run)"`
}

// campaignPendingAmendments mirrors the backend's campaignPendingAmendmentsPayload:
// every pending amendment on any run of the campaign, oldest requested_at first.
type campaignPendingAmendments struct {
	Items     []campaignPendingAmendment `json:"items" jsonschema:"every pending scope amendment on any run of the campaign (each item's linked run plus its recovery and decomposition descendants), oldest requested_at first"`
	Truncated bool                       `json:"truncated" jsonschema:"true when the campaign's run set exceeded the backend's enumeration cap, so some runs were not scanned"`
}

// runCampaignPendingAmendments mirrors the backend's
// runCampaignPendingAmendmentsResponse. CampaignID is nil when the run is in no
// campaign (the backend writes JSON null, not an omitted key).
type runCampaignPendingAmendments struct {
	RunID      string                     `json:"run_id"`
	CampaignID *string                    `json:"campaign_id"`
	Items      []campaignPendingAmendment `json:"items"`
	Truncated  bool                       `json:"truncated"`
}

// GetRunCampaignPendingAmendments wraps GET
// /v0/runs/{run_id}/campaign-pending-amendments with the operator bearer. The
// backend refuses a run-bound token (403 run_token_forbidden) and a token
// without read:audit (403 insufficient_scope).
func (c *apiClient) GetRunCampaignPendingAmendments(ctx context.Context, runID uuid.UUID) (*runCampaignPendingAmendments, error) {
	var out runCampaignPendingAmendments
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/campaign-pending-amendments", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// campaignLiveAmendment returns the oldest DECISION-LIVE pending amendment on
// any run of runID's campaign that skip does not exclude, plus the campaign id.
//
// BEST-EFFORT by construction, mirroring awaitStagePendingAmendment: any client
// error (an older backend without the route, a token without read:audit, a
// transient 5xx), a nil or empty campaign_id, or no surviving entry returns
// (nil, ""), so the caller's normal poll/timeout path stays in charge and the
// wait never fails on this probe.
//
// A not-live entry (its stage settled with the request still pending, e.g. a
// failed stage #4127 deliberately keeps recoverable) is skipped: releasing on
// it would wedge every wait on the campaign until someone decided a request no
// agent is waiting on. It stays visible on fishhawk_get_campaign_status.
//
// The backend returns items oldest-first, so the first survivor is the request
// closest to expiring undecided.
func (r *runResolver) campaignLiveAmendment(ctx context.Context, runID uuid.UUID, skip func(*campaignPendingAmendment) bool) (*campaignPendingAmendment, string) {
	resp, err := r.api.GetRunCampaignPendingAmendments(ctx, runID)
	if err != nil || resp == nil || resp.CampaignID == nil || *resp.CampaignID == "" {
		return nil, ""
	}
	for i := range resp.Items {
		e := &resp.Items[i]
		if !e.DecisionLive {
			continue
		}
		if skip != nil && skip(e) {
			continue
		}
		return e, *resp.CampaignID
	}
	return nil, ""
}

// campaignAmendmentNextStep is the pre-filled fishhawk_decide_scope_amendment
// call for a campaign-wide release. run_id is the FILING run
// (e.Amendment.RunID), never the awaited run: the decision route is keyed on the
// run that owns the amendment.
func campaignAmendmentNextStep(e *campaignPendingAmendment) *SuggestedAction {
	return &SuggestedAction{
		Action: "fishhawk_decide_scope_amendment",
		Params: map[string]string{
			"run_id":       e.Amendment.RunID,
			"amendment_id": e.Amendment.ID,
		},
		Precondition: "another run of this campaign filed the scope amendment, it is still pending and its stage has not settled",
		Consumes:     "none",
		Reason: "that run's agent is blocked on this decision and its " + amendmentPollWindowText() + " poll window expires UNDECIDED " +
			"(an expiry is not a denial — the request stays pending until its stage succeeds, is cancelled or superseded, " +
			"or its run succeeds or is cancelled, which lapses it, #4127; a failed stage or failed run stays recoverable)",
	}
}

// campaignAmendmentMessage is the operator message for a campaign-wide
// release. awaited names what the caller is blocked on ("stage \"implement\" of
// run X", "the fan-out of run Y"), which has NOT released for its own reasons.
func campaignAmendmentMessage(e *campaignPendingAmendment, campaignID, awaited string) string {
	return fmt.Sprintf("run %s (campaign item %s, campaign %s) filed scope amendment %s for %s and it is still pending. Reason: %s. "+
		"%s has NOT released for its own reasons: this release surfaces a decision ANOTHER run of the campaign is blocked on. "+
		"Decide it now with fishhawk_decide_scope_amendment (next_step pre-fills the FILING run's run_id + amendment_id): "+
		"its agent polls %s per request and then the request EXPIRES UNDECIDED (it stays pending — an expiry is not a denial). "+
		"Then re-arm this wait.",
		e.Amendment.RunID, e.IssueRef, campaignID, e.Amendment.ID, amendmentPathList(&e.Amendment), e.Amendment.Reason,
		awaited, amendmentPollWindowText())
}
