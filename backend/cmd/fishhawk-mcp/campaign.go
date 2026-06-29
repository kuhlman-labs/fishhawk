package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- fishhawk_start_campaign (E25.8 / #1447) ---

// StartCampaignInput is the fishhawk_start_campaign tool's input schema. repo +
// epic_ref are required; pause_policy is optional (empty normalizes to
// pause_campaign server-side).
type StartCampaignInput struct {
	Repo        string `json:"repo" jsonschema:"GitHub repo as owner/name to assemble the campaign in"`
	EpicRef     string `json:"epic_ref" jsonschema:"the epic reference to decompose into the campaign DAG (e.g. an issue ref like '#25' or 'owner/name#25')"`
	PausePolicy string `json:"pause_policy,omitempty" jsonschema:"OPTIONAL pause behavior on a gate hand-off: 'pause_campaign' (block the whole campaign, the default) or 'pause_item' (continue-others). Omit to take the conservative pause_campaign default"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent override. Typed
	// map[string]any so the MCP SDK's reflection-built tool input schema sees an
	// unconstrained object (the agent passes the operator_agent block as JSON);
	// the backend validates it against spec.OperatorAgent (unknown fields ->
	// 400). When present (including an explicit empty {}) it wins WHOLESALE over
	// each issue-run's workflow operator_agent contract. An explicit {} means the
	// wholesale override with no delegated knobs — page on every action. Omit
	// (nil map) to leave every issue-run on its workflow default.
	OperatorAgent map[string]any `json:"operator_agent,omitempty" jsonschema:"OPTIONAL campaign-level operator_agent delegation override. A JSON object with the operator_agent knobs (may_approve, may_route_fixup, may_waive, may_retry, may_merge, must_page_human, model_policy). When set it REPLACES (wins wholesale over) every issue-run's per-workflow operator_agent contract for the whole campaign — it is never merged. An explicit empty {} is a valid wholesale override with no delegated knobs (page on every action). Omit to leave each issue-run on its workflow default"`
}

// StartCampaignOutput carries the created campaign row.
type StartCampaignOutput struct {
	Campaign Campaign `json:"campaign"`
}

// GetCampaignStatusInput is the fishhawk_get_campaign_status tool's input.
type GetCampaignStatusInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"the campaign UUID (from fishhawk_start_campaign)"`
}

// GetCampaignStatusOutput is the campaign rollup surface: the campaign + items +
// readiness rollup + the server-computed next_action, PLUS next_actions — the
// MCP classifier's mapping of that next_action onto a legal operator action so
// the agent never reads an unclassified campaign state.
type GetCampaignStatusOutput struct {
	Campaign    Campaign           `json:"campaign"`
	Items       []CampaignItem     `json:"items"`
	Rollup      CampaignRollup     `json:"rollup"`
	NextAction  CampaignNextAction `json:"next_action" jsonschema:"the server-computed next step distilled from the rollup: action is one of attention, resume, start_run, wait, complete"`
	NextActions *NextActions       `json:"next_actions,omitempty" jsonschema:"the MCP classifier's mapping of next_action onto a legal operator action (the tool to call, its precondition, what it consumes, and a one-line reason). Non-empty for every non-complete campaign; nil-actions on a complete campaign. Display-only"`
}

// ResumeCampaignInput is the fishhawk_resume_campaign tool's input.
type ResumeCampaignInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"the paused campaign's UUID to hand back to the auto-driver"`
}

// ResumeCampaignOutput carries the updated (resumed) campaign row.
type ResumeCampaignOutput struct {
	Campaign Campaign `json:"campaign"`
}

// registerStartCampaign wires the fishhawk_start_campaign tool (E25.8 / #1447).
//
// Auth: a write tool — operator-side fhk_* tokens with scope write:campaigns
// (the backend handler calls requireWriteScope("write:campaigns")). A
// runner-bound fhm_* token surfaces a 403 as a tool error.
func registerStartCampaign(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_start_campaign",
		Description: strings.TrimSpace(`
Start a campaign from an epic. Use this when you want the operator-agent to
drive a whole epic's child issues as a dependency-ordered campaign rather than
starting each run by hand — the campaign counterpart to fishhawk_start_run
(which opens a single run). It queries the epic's children + depends_on edges,
wave-orders them into a DAG, and persists the campaign; poll it afterwards with
fishhawk_get_campaign_status and hand a paused campaign back with
fishhawk_resume_campaign.

repo (owner/name) and epic_ref are required. pause_policy is optional —
pause_campaign (the default, block the whole campaign at a gate hand-off) or
pause_item (continue the other items). operator_agent is optional — a
campaign-level operator_agent delegation block that REPLACES (wins wholesale
over) every issue-run's per-workflow operator_agent contract for the whole
campaign; an explicit empty {} is a valid wholesale override with no delegated
knobs (page on every action); omit to leave each issue-run on its workflow
default. A write tool:
needs an operator token with write:campaigns scope (a runner-bound token is
rejected 403). An epic whose dependency edges point outside its own children
fails campaign_dangling_dependency; a repo without the GitHub App installed
fails repo_not_installed; a malformed or unknown-field operator_agent fails
validation_failed.
`),
	}, resolver.startCampaign)
}

// startCampaign is the tool handler.
func (r *runResolver) startCampaign(ctx context.Context, _ *mcp.CallToolRequest, in StartCampaignInput) (*mcp.CallToolResult, StartCampaignOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		return nil, StartCampaignOutput{}, errors.New("repo is required (owner/name)")
	}
	if strings.TrimSpace(in.EpicRef) == "" {
		return nil, StartCampaignOutput{}, errors.New("epic_ref is required")
	}

	// Marshal the OPTIONAL campaign-level operator_agent override back to opaque
	// JSON for the request body. Presence (non-nil map) is the discriminator:
	// encoding/json leaves an omitted field as a nil map but unmarshals an
	// explicit {} into a non-nil empty map, so != nil correctly distinguishes the
	// two. An omitted override (nil) stays nil so CreateCampaign omits the field
	// and the campaign inherits each issue-run's workflow contract. A present
	// override — even the empty map {} (wholesale override: no delegated knobs,
	// page on every action) — is marshaled and carried verbatim to the REST
	// layer; an empty map marshals to the two-byte "{}", which the request
	// body's json.RawMessage omitempty field preserves (omitempty drops only nil
	// and zero-length byte slices, not a populated "{}"). The backend is the
	// validation authority; we only carry the bytes.
	var operatorAgent json.RawMessage
	if in.OperatorAgent != nil {
		b, err := json.Marshal(in.OperatorAgent)
		if err != nil {
			return nil, StartCampaignOutput{}, fmt.Errorf("operator_agent is not encodable as JSON: %w", err)
		}
		operatorAgent = b
	}

	created, err := r.api.CreateCampaign(ctx, repo, in.EpicRef, in.PausePolicy, operatorAgent)
	if err != nil {
		// Map the backend's gate codes onto operator-actionable tool errors.
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "repo_not_installed":
				return nil, StartCampaignOutput{}, fmt.Errorf(
					"repo_not_installed: %s — install the Fishhawk GitHub App on %s before starting a campaign", ae.Message, repo)
			case "campaign_dangling_dependency":
				return nil, StartCampaignOutput{}, fmt.Errorf(
					"campaign_dangling_dependency: %s — an epic child declares a depends_on that is not a fellow child of %s; fix the epic's dependency edges and retry", ae.Message, in.EpicRef)
			case "campaign_repo_unconfigured":
				return nil, StartCampaignOutput{}, fmt.Errorf(
					"campaign_repo_unconfigured: %s — this deployment has no campaign repository wired, so campaigns cannot be created", ae.Message)
			}
		}
		return nil, StartCampaignOutput{}, fmt.Errorf("create campaign: %w", err)
	}
	return nil, StartCampaignOutput{Campaign: *created}, nil
}

// registerGetCampaignStatus wires the fishhawk_get_campaign_status tool (read-only).
func registerGetCampaignStatus(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_get_campaign_status",
		Description: strings.TrimSpace(`
Snapshot a campaign's progress in one call — the operator-agent's "what does
the campaign need next" query. Use this after fishhawk_start_campaign and on
every drive tick: it returns the campaign row, its items, the engine's
readiness rollup (eligible/blocked/running/done/failed/cancelled/paused), the
server-computed next_action, and a next_actions block mapping that next_action
onto a legal operator move (start the next eligible run, resume a paused
campaign, attend a failed item, or wait). The campaign analogue of
fishhawk_get_run_status. Read-only.
`),
	}, resolver.getCampaignStatus)
}

// getCampaignStatus is the tool handler.
func (r *runResolver) getCampaignStatus(ctx context.Context, _ *mcp.CallToolRequest, in GetCampaignStatusInput) (*mcp.CallToolResult, GetCampaignStatusOutput, error) {
	id, err := uuid.Parse(in.CampaignID)
	if err != nil {
		return nil, GetCampaignStatusOutput{}, fmt.Errorf("campaign_id %q is not a valid UUID: %w", in.CampaignID, err)
	}

	st, err := r.api.GetCampaignStatus(ctx, id)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "campaign_not_found" {
			return nil, GetCampaignStatusOutput{}, fmt.Errorf(
				"campaign_not_found: no campaign with id %s — pass the id fishhawk_start_campaign returned", id)
		}
		return nil, GetCampaignStatusOutput{}, fmt.Errorf("get campaign status: %w", err)
	}

	return nil, GetCampaignStatusOutput{
		Campaign:    st.Campaign,
		Items:       st.Items,
		Rollup:      st.Rollup,
		NextAction:  st.NextAction,
		NextActions: campaignNextActionsFor(st.Rollup, st.NextAction),
	}, nil
}

// registerResumeCampaign wires the fishhawk_resume_campaign tool (E25.7 hand-back).
//
// Auth: a write tool — operator-side fhk_* tokens with write:campaigns scope.
func registerResumeCampaign(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_resume_campaign",
		Description: strings.TrimSpace(`
Hand a paused campaign back to the auto-driver. Use this when the campaign's
next_action is "resume" — the driver paged a human at a run gate (E25.7) and
the campaign (or an item) is paused awaiting that hand-off. Once you have
handled the gate, this flips the paused campaign and every paused item back to
running so the next driver tick re-engages. The campaign counterpart to
fishhawk_resume_run. A write tool: needs an operator token with write:campaigns
scope. When nothing is paused on either axis the backend returns
campaign_not_paused (there is nothing to resume).
`),
	}, resolver.resumeCampaign)
}

// resumeCampaign is the tool handler.
func (r *runResolver) resumeCampaign(ctx context.Context, _ *mcp.CallToolRequest, in ResumeCampaignInput) (*mcp.CallToolResult, ResumeCampaignOutput, error) {
	id, err := uuid.Parse(in.CampaignID)
	if err != nil {
		return nil, ResumeCampaignOutput{}, fmt.Errorf("campaign_id %q is not a valid UUID: %w", in.CampaignID, err)
	}

	updated, err := r.api.ResumeCampaign(ctx, id)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "campaign_not_paused":
				return nil, ResumeCampaignOutput{}, fmt.Errorf(
					"campaign_not_paused: nothing to resume — no item and not the campaign itself is paused on campaign %s. Poll fishhawk_get_campaign_status: a resume is only legal when the next_action is 'resume'", id)
			case "campaign_not_found":
				return nil, ResumeCampaignOutput{}, fmt.Errorf(
					"campaign_not_found: no campaign with id %s — pass the id fishhawk_start_campaign returned", id)
			}
		}
		return nil, ResumeCampaignOutput{}, fmt.Errorf("resume campaign: %w", err)
	}
	return nil, ResumeCampaignOutput{Campaign: *updated}, nil
}
