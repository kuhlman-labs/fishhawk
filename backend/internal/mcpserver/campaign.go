package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- fishhawk_start_campaign (E25.8 / #1447) ---

// StartCampaignInput is the fishhawk_start_campaign tool's input schema. repo is
// required; ONE of epic_ref / items must be present (epic_ref decomposes an epic,
// items assembles a no-epic campaign over an explicit issue list — #2051).
// pause_policy is optional (empty normalizes to pause_campaign server-side).
type StartCampaignInput struct {
	Repo        string `json:"repo" jsonschema:"GitHub repo as owner/name to assemble the campaign in"`
	EpicRef     string `json:"epic_ref,omitempty" jsonschema:"OPTIONAL the epic reference to decompose into the campaign DAG (e.g. an issue ref like '#25' or 'owner/name#25'). Omit it and pass items to assemble a no-epic campaign over an explicit issue list instead; one of epic_ref / items is required"`
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
	// Items is the OPTIONAL subset filter (#2003) / no-epic item list (#2051):
	// WITH epic_ref it scopes the campaign to a named subset of the epic's
	// children; WITHOUT epic_ref it is the authoritative issue set the no-epic
	// campaign assembles over.
	Items []string `json:"items,omitempty" jsonschema:"OPTIONAL issue refs (a bare number like '101' or 'issue:101'). WITH epic_ref: the subset of the epic's children to scope the campaign to — every item must be a child of the epic (a non-child fails campaign_item_not_child); omit to sweep every child. WITHOUT epic_ref: the authoritative issue set a no-epic campaign assembles over, resolving each issue's depends_on directly (#2051). In both modes an included item whose depends_on points at an issue OUTSIDE the set fails campaign_dangling_dependency (that dependency must run within the batch). One of epic_ref / items is required"`
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

// CancelCampaignInput is the fishhawk_cancel_campaign tool's input.
type CancelCampaignInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"the campaign UUID to cancel (marks it and its unfinished items cancelled)"`
}

// CancelCampaignOutput carries the updated (cancelled) campaign row.
type CancelCampaignOutput struct {
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

repo (owner/name) is required, plus ONE of epic_ref / items. pause_policy is
optional — pause_campaign (the default, block the whole campaign at a gate
hand-off) or pause_item (continue the other items). Two ways to scope the batch:
pass epic_ref to decompose an epic's children, optionally narrowed by items (a
subset of the epic's child issue refs — every item must be a child of the epic,
the DAG is built over just those items); OR omit epic_ref and pass items alone to
assemble a NO-EPIC campaign over exactly that issue list (#2051), resolving each
issue's depends_on directly with no shared epic parent. In both modes an included
item whose depends_on points at an issue outside the set fails
campaign_dangling_dependency (that dependency must run within the batch); with
epic_ref, a non-child item fails campaign_item_not_child. operator_agent is optional — a
campaign-level operator_agent delegation block that REPLACES (wins wholesale
over) every issue-run's per-workflow operator_agent contract for the whole
campaign; an explicit empty {} is a valid wholesale override with no delegated
knobs (page on every action); omit to leave each issue-run on its workflow
default. A write tool:
needs an operator token with write:campaigns scope (a runner-bound token is
rejected 403). An epic whose dependency edges point outside its own children
fails campaign_dangling_dependency; a requested item that is not a child of the
epic fails campaign_item_not_child; a repo without the GitHub App installed
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
	// epic_ref is OPTIONAL as of #2051; require epic_ref OR a non-empty items
	// list (pass epic_ref to decompose an epic, or items alone for the no-epic
	// variant). The server owns the branch decision — the client forwards both.
	if strings.TrimSpace(in.EpicRef) == "" && len(in.Items) == 0 {
		return nil, StartCampaignOutput{}, errors.New("one of epic_ref or items is required: pass epic_ref to decompose an epic, or items alone to assemble a no-epic campaign over an explicit issue list")
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

	created, err := r.api.CreateCampaign(ctx, repo, in.EpicRef, in.PausePolicy, operatorAgent, in.Items)
	if err != nil {
		// Map the backend's gate codes onto operator-actionable tool errors.
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "repo_not_installed":
				return nil, StartCampaignOutput{}, fmt.Errorf(
					"repo_not_installed: %s — install the Fishhawk GitHub App on %s before starting a campaign", ae.Message, repo)
			case "campaign_item_not_child":
				return nil, StartCampaignOutput{}, fmt.Errorf(
					"campaign_item_not_child: %s — an items ref is not a child of epic %s; pass only issue refs that are children of the epic, or omit items to sweep every child", ae.Message, in.EpicRef)
			case "campaign_dangling_dependency":
				// Branch the operator remedy on which categories the backend
				// enriched into details (#2120): an out-of-epic target keeps the
				// "not a fellow child" fix-the-edges wording; an excluded-but-
				// incomplete subset sibling names the include-in-items / omit-items
				// remedy. Render both when both are present; fall back to the
				// not_child wording when details are absent (older backend).
				_, notChild := ae.Details["dangling_not_child"]
				_, excluded := ae.Details["dangling_excluded_incomplete"]
				const notChildRemedy = "an epic child declares a depends_on that is not a fellow child of %s; fix the epic's dependency edges and retry"
				const excludedRemedy = "an included item's depends_on targets a sibling excluded from items that is not yet complete; include it in items, or omit items to sweep every child so a completed dependency auto-settles"
				switch {
				case excluded && notChild:
					return nil, StartCampaignOutput{}, fmt.Errorf(
						"campaign_dangling_dependency: %s — two causes: (1) "+notChildRemedy+"; (2) "+excludedRemedy, ae.Message, in.EpicRef)
				case excluded:
					return nil, StartCampaignOutput{}, fmt.Errorf(
						"campaign_dangling_dependency: %s — "+excludedRemedy, ae.Message)
				default:
					return nil, StartCampaignOutput{}, fmt.Errorf(
						"campaign_dangling_dependency: %s — "+notChildRemedy, ae.Message, in.EpicRef)
				}
			case "campaign_repo_unconfigured":
				return nil, StartCampaignOutput{}, fmt.Errorf(
					"campaign_repo_unconfigured: %s — this deployment has no campaign repository wired, so campaigns cannot be created", ae.Message)
			}
		}
		return nil, StartCampaignOutput{}, fmt.Errorf("create campaign: %w", err)
	}
	return nil, StartCampaignOutput{Campaign: *created}, nil
}

// --- fishhawk_start_campaign_item_run (E26.2 / #1481) ---

// StartCampaignItemRunInput is the fishhawk_start_campaign_item_run tool's
// input. campaign_id + issue_ref + workflow_id are required; workflow_ref,
// runner_kind and working_dir are optional (working_dir is REQUIRED when
// runner_kind is local — see startCampaignItemRun's guards). There is
// deliberately no idempotency_key — the backend does not dedup this start, and
// the DAG eligibility gate already refuses a re-start against an already-running
// item.
type StartCampaignItemRunInput struct {
	CampaignID  string `json:"campaign_id" jsonschema:"the campaign UUID (from fishhawk_start_campaign)"`
	IssueRef    string `json:"issue_ref" jsonschema:"the campaign item's issue ref to start (must be one of the campaign's items and currently startable per the DAG: either eligible, or a deps-satisfied cancelled item, which is restarted with a fresh run)"`
	WorkflowID  string `json:"workflow_id" jsonschema:"the workflow id to run for this issue (e.g. 'feature_change')"`
	WorkflowRef string `json:"workflow_ref,omitempty" jsonschema:"OPTIONAL git ref to fetch the workflow spec at; omit for the repo's default branch"`
	RunnerKind  string `json:"runner_kind,omitempty" jsonschema:"OPTIONAL execution backend: 'github_actions' (default) or 'local'. Pass 'local' for the local dogfood loop so the run executes through the local runner"`
	// WorkingDir binds the minted run's local checkout (E48.69 / #2498).
	WorkingDir string `json:"working_dir,omitempty" jsonschema:"absolute path to the checkout this campaign item's run executes in. REQUIRED when runner_kind is local; bound to the minted run so run_stage/dispatch_stage/run_children/drive_run all inherit it with no per-call repetition. YOU, the calling agent, resolve your own checkout (you are running inside one) rather than asking the operator for a path. A non-absolute value is refused for any runner_kind"`
}

// StartCampaignItemRunOutput carries the minted run plus the linked campaign
// item (now running, with run_id set).
type StartCampaignItemRunOutput struct {
	Run  Run          `json:"run"`
	Item CampaignItem `json:"item"`
	// Elisions is the ADR-077 byte-bound block (#2510). This is the verb that
	// concretely broke on 2026-08-07 at 75,963 characters — a mutating verb
	// whose run had ALREADY been minted when the response failed to render — so
	// the row is reduced rather than the response rejected.
	Elisions *Elisions `json:"elisions,omitempty" jsonschema:"present only when the run row was reduced to fit the tool-result byte budget: the effective budget, the deepest tier applied, and one entry per omission with its class and the retrieval surface that returns AT LEAST the omitted content"`
}

// registerStartCampaignItemRun wires the fishhawk_start_campaign_item_run tool
// (E26.2 / #1481).
//
// Auth: a write tool — operator-side fhk_* tokens with scope write:campaigns
// (the backend handler calls requireWriteScope("write:campaigns")). A
// runner-bound fhm_* token surfaces a 403 as a tool error.
func registerStartCampaignItemRun(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_start_campaign_item_run",
		Description: strings.TrimSpace(`
Start a run for one startable campaign item and link it to the campaign. Use
this when fishhawk_get_campaign_status reports next_action "start_run" — to drive
a campaign locally yourself, instead of the backend auto-driver, so the campaign
tracks + DAG-gates each run as you push it to merge. It admits an item that is
either eligible (its dependencies have all succeeded) OR a deps-satisfied
cancelled item, which it RESTARTS: the cancelled item is reset to pending and a
fresh run is minted, re-linked, and audited (campaign_issue_restarted) so its
dependents no longer stay blocked forever. On refusal it names the blocking
dependency; then it mints the run, links it, and moves the item to running. Poll
fishhawk_get_campaign_status again after starting — the status read settles each
run as it reaches terminal and advances the campaign in DAG order.

campaign_id, issue_ref, and workflow_id are required. Pass runner_kind 'local'
for the local dogfood loop; for a local item working_dir is REQUIRED — the
absolute path to the checkout this item's run executes in (you resolve your own
checkout, you are running inside one). working_dir binds the minted run, so
run_stage/dispatch_stage/run_children/drive_run for that item all inherit it and
you pass it once. An omitted working_dir on a local item is refused rather than
falling through to the client's cwd; a non-absolute working_dir is refused for
any runner_kind. A write tool: needs an operator token with
write:campaigns scope (a runner-bound token is rejected 403). A running or
still-blocked item fails item_not_eligible (the detail names the unmet
dependency); a deps-satisfied autonomy:low (human-led) item fails item_human_led
instead — a human must lead it out of band, do not start an agent run; an
unknown issue_ref fails campaign_item_not_found; a paused or terminal campaign
fails campaign_not_startable.
`),
	}, resolver.startCampaignItemRun)
}

// startCampaignItemRun is the tool handler.
func (r *runResolver) startCampaignItemRun(ctx context.Context, req *mcp.CallToolRequest, in StartCampaignItemRunInput) (*mcp.CallToolResult, StartCampaignItemRunOutput, error) {
	id, err := uuid.Parse(in.CampaignID)
	if err != nil {
		return nil, StartCampaignItemRunOutput{}, fmt.Errorf("campaign_id %q is not a valid UUID: %w", in.CampaignID, err)
	}
	if strings.TrimSpace(in.IssueRef) == "" {
		return nil, StartCampaignItemRunOutput{}, errors.New("issue_ref is required")
	}
	if strings.TrimSpace(in.WorkflowID) == "" {
		return nil, StartCampaignItemRunOutput{}, errors.New("workflow_id is required")
	}

	// working_dir guards (E48.69 / #2498), fired BEFORE the r.api round-trip so
	// a refusal dials no backend and mints no run. Both guards are transport-
	// INDEPENDENT — the issue's proposal 3 asks for a loud refusal rather than
	// the stdio cwd fall-through, so unlike start_run's HTTP-only admission
	// check these fire on stdio too.
	workingDir := strings.TrimSpace(in.WorkingDir)
	if workingDir == "" && in.RunnerKind == driveRunnerKindLocal {
		// A local item binds the checkout every later runner-spawning verb
		// inherits, so an omitted value is refused — never a silent fall-through
		// to the client process's own cwd (the #1866 contamination shape).
		return nil, StartCampaignItemRunOutput{}, errors.New(
			"working_dir is required for a local campaign item run: resolve your own checkout (you are running inside one) and pass its absolute path — it binds the minted run and every later stage verb inherits it")
	}
	if workingDir != "" && !filepath.IsAbs(workingDir) {
		// Refused for ANY runner_kind: a relative value would resolve against
		// the fishhawkd host's cwd, not the operator's project, and the run's
		// persisted binding would be poisoned.
		return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
			"working_dir %q must be an absolute path: a relative path resolves against the fishhawkd host's cwd, not your project, so it is refused for any runner_kind; resolve your own checkout and pass its absolute path — it binds the minted run and every later stage inherits it", workingDir)
	}
	if workingDir != "" {
		workingDir = filepath.Clean(workingDir)
	}

	res, err := r.api.StartCampaignItemRun(ctx, id, in.IssueRef, in.WorkflowID, in.WorkflowRef, in.RunnerKind, workingDir)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "campaign_not_found":
				return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
					"campaign_not_found: no campaign with id %s — pass the id fishhawk_start_campaign returned", id)
			case "campaign_item_not_found":
				return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
					"campaign_item_not_found: %s — no campaign item with issue_ref %q; read its items via fishhawk_get_campaign_status", ae.Message, in.IssueRef)
			case "item_not_eligible":
				return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
					"item_not_eligible: %s — only an eligible item can be started; poll fishhawk_get_campaign_status and start the ref its next_action names", ae.Message)
			case "item_human_led":
				return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
					"item_human_led: %s — this item is deps-satisfied but autonomy:low (human-led); a human must lead it out of band (do not start an agent run), then re-poll fishhawk_get_campaign_status", ae.Message)
			case "campaign_not_startable":
				return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
					"campaign_not_startable: %s — a paused campaign must be resumed (fishhawk_resume_campaign) and a terminal one cannot start new runs", ae.Message)
			case "campaign_run_start_failed":
				return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
					"campaign_run_start_failed: %s — could not resolve the installation or workflow spec; ensure the GitHub App is installed and the workflow_id exists at workflow_ref", ae.Message)
			case "campaign_repo_unconfigured":
				return nil, StartCampaignItemRunOutput{}, fmt.Errorf(
					"campaign_repo_unconfigured: %s — this deployment has no campaign repository wired", ae.Message)
			}
		}
		return nil, StartCampaignItemRunOutput{}, fmt.Errorf("start campaign item run: %w", err)
	}
	bounded, berr := boundRunRowOutput(
		StartCampaignItemRunOutput{Run: res.Run, Item: res.Item}, res.Run.ID, r.responseBudget(req),
		func(o *StartCampaignItemRunOutput) []*Run { return []*Run{&o.Run} },
		func(o *StartCampaignItemRunOutput, e *Elisions) { o.Elisions = e })
	if berr != nil {
		return nil, StartCampaignItemRunOutput{}, fmt.Errorf("bound start_campaign_item_run response: %w", berr)
	}
	return nil, bounded, nil
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

// registerCancelCampaign wires the fishhawk_cancel_campaign tool (#2355).
//
// Auth: a write tool — operator-side fhk_* tokens with write:campaigns scope.
func registerCancelCampaign(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_cancel_campaign",
		Description: strings.TrimSpace(`
Cancel a campaign. Use this to cleanly shut down an abandoned or rebuilt
campaign so it stops showing as live work in the campaign list. It marks the
campaign AND every one of its unfinished (non-terminal) items cancelled, so an
orphaned campaign left in 'running' from a rebuild workaround no longer looks
indistinguishable from a real in-flight campaign.

It deliberately does NOT cancel the campaign's linked RUNS — a run still in
flight keeps running. Cancel individual runs with fishhawk_cancel_run; this verb
only cancels the campaign and its item rows. A write tool: needs an operator
token with write:campaigns scope (a runner-bound token is rejected 403). An
already-terminal campaign (succeeded/failed/cancelled) returns
campaign_not_cancellable; an unknown id returns campaign_not_found. The verb is
idempotent and convergent — re-invoking after a partial failure completes the
cancellation.
`),
	}, resolver.cancelCampaign)
}

// cancelCampaign is the tool handler.
func (r *runResolver) cancelCampaign(ctx context.Context, _ *mcp.CallToolRequest, in CancelCampaignInput) (*mcp.CallToolResult, CancelCampaignOutput, error) {
	id, err := uuid.Parse(in.CampaignID)
	if err != nil {
		return nil, CancelCampaignOutput{}, fmt.Errorf("campaign_id %q is not a valid UUID: %w", in.CampaignID, err)
	}

	updated, err := r.api.CancelCampaign(ctx, id)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "campaign_not_cancellable":
				return nil, CancelCampaignOutput{}, fmt.Errorf(
					"campaign_not_cancellable: campaign %s is already terminal (succeeded/failed/cancelled) — there is nothing to cancel", id)
			case "campaign_not_found":
				return nil, CancelCampaignOutput{}, fmt.Errorf(
					"campaign_not_found: no campaign with id %s — pass the id fishhawk_start_campaign returned", id)
			}
		}
		return nil, CancelCampaignOutput{}, fmt.Errorf("cancel campaign: %w", err)
	}
	return nil, CancelCampaignOutput{Campaign: *updated}, nil
}
