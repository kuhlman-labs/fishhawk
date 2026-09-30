package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
)

// fishhawk_delegation_confirm (E76.5 / #3768, ADR-083 #3751 rule 7) is the
// MCP half of delegation confirmation on handover: a thin wrapper over
//
//	GET  /v0/repos/{owner}/{name}/delegation/confirmation   (action=read)
//	POST /v0/repos/{owner}/{name}/delegation/confirm        (action=confirm)
//	POST /v0/repos/{owner}/{name}/delegation/lower          (action=lower)
//
// The action set is CLOSED — {read, confirm, lower}, exactly
// delegationconfirm.Actions() — and there is no raise action. The input
// schema is SUPPLIED (delegationConfirmInputSchema), not merely reflected, so
// it can advertise the closed action enum and restrict every tier-bearing
// field (proposed_tier, proposed_escalation.max_autonomy) to {low, medium}:
// "high" is never strictly lower than any tier, so the schema cannot name it.
// Every refusal beyond that is the backend's — this tool neither re-derives
// the confirmation state nor re-checks the captain, so an agent calling a
// write action is refused by the backend's single guard
// (delegation_agent_identity_refused), never by a second copy here.
//
// The I/O types are UNEXPORTED: the SDK's schema reflection needs exported
// FIELDS, not exported type names, so the package's pinned export surface
// (export_surface_test.go) does not grow for this tool.
//
// The response is bounded per ADR-077 through the SHARED machinery
// (elisionLedger, attachAndMeasureOut, marshalledLen, capJSONString,
// pointerREST) — see boundDelegationConfirmOutput.

// delegationLowerTiers is the only tier vocabulary a lower proposal can
// name: the tiers that can be strictly below some other tier.
var delegationLowerTiers = []any{"low", "medium"}

// delegationConfirmInput is the tool's input schema.
type delegationConfirmInput struct {
	Repo               string                        `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	Action             string                        `json:"action,omitempty" jsonschema:"one of read (default — writes nothing), confirm, lower. There is no raise action"`
	Workflow           string                        `json:"workflow,omitempty" jsonschema:"confirm and lower: the workflow id. Refused on read"`
	ContentHash        string                        `json:"content_hash,omitempty" jsonschema:"confirm only: the workflow's OWN content_hash exactly as you were shown it (fishhawk_delegation or action=read current_content_hash). A hash that is no longer current is refused (delegation_hash_stale)"`
	ProposedTier       string                        `json:"proposed_tier,omitempty" jsonschema:"lower only: the proposed autonomy tier — must be STRICTLY below the workflow's current tier"`
	ProposedEscalation *delegationconfirm.Escalation `json:"proposed_escalation,omitempty" jsonschema:"lower only: an escalation to append — paths plus a max_autonomy at or below the effective proposed tier"`
	Reason             string                        `json:"reason,omitempty" jsonschema:"lower only: why the delegation should be lowered (required)"`
	Source             string                        `json:"source,omitempty" jsonschema:"where to read the spec from: ref (default, through the forge at ref) or run_cache (the spec cached on the newest run). Echoed on read, never switched silently"`
	Ref                string                        `json:"ref,omitempty" jsonschema:"the git ref to read the spec at under source=ref; omit for the default branch head"`
	ParentEpic         string                        `json:"parent_epic,omitempty" jsonschema:"lower only: the parent epic the filed work item links to"`
	TitleVars          map[string]string             `json:"title_vars,omitempty" jsonschema:"lower only: title placeholders the repository's work-item conventions require (e.g. epic, n)"`
	Labels             []string                      `json:"labels,omitempty" jsonschema:"lower only: extra labels for the filed work item; any autonomy:* label is replaced by autonomy:low"`
}

// delegationConfirmEvent is the recorded chain entry.
type delegationConfirmEvent struct {
	Sequence  int64          `json:"sequence"`
	EntryHash string         `json:"entry_hash"`
	Category  string         `json:"category" jsonschema:"delegation_confirmed or delegation_lower_proposed"`
	At        time.Time      `json:"at"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// delegationConfirmFiled is the lower action's filed work item.
type delegationConfirmFiled struct {
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
}

// delegationConfirmationState mirrors the GET .../delegation/confirmation
// body.
type delegationConfirmationState struct {
	Repo                 string                             `json:"repo"`
	Source               string                             `json:"source"`
	Ref                  string                             `json:"ref,omitempty"`
	WorkflowSHA          string                             `json:"workflow_sha,omitempty"`
	Captain              *string                            `json:"captain" jsonschema:"the captain in force; null when the seat is vacant"`
	SeatSequence         int64                              `json:"seat_sequence" jsonschema:"the chain sequence of the latest seat change; 0 when no captain has ever sat"`
	Workflows            []delegationconfirm.WorkflowStatus `json:"workflows" jsonschema:"every workflow of the delegation view with its verdict: confirmed, unconfirmed (reason handover or hash_stale) or no_captain"`
	UnconfirmedWorkflows []string                           `json:"unconfirmed_workflows"`
	SkippedEntries       int                                `json:"skipped_entries"`
	IgnoredEntries       int                                `json:"ignored_entries" jsonschema:"well-formed entries whose actor was not the captain in force when they landed"`
}

// delegationConfirmVerbResult mirrors both write routes' 200 body.
type delegationConfirmVerbResult struct {
	Repo     string                           `json:"repo"`
	Workflow delegationconfirm.WorkflowStatus `json:"workflow" jsonschema:"the workflow's verdict after the entry was recorded. A lower proposal does NOT confirm"`
	Event    delegationConfirmEvent           `json:"event" jsonschema:"the chain entry this action appended"`
	Filed    *delegationConfirmFiled          `json:"filed,omitempty" jsonschema:"lower only: the autonomy:low work item proposing the workflows.yaml edit"`
}

// delegationConfirmOutput is the tool's result: exactly one of Confirmation
// (action=read) or Result (a write action) is set.
type delegationConfirmOutput struct {
	Confirmation *delegationConfirmationState `json:"confirmation,omitempty" jsonschema:"read action: every workflow's confirmation verdict"`
	Result       *delegationConfirmVerbResult `json:"result,omitempty" jsonschema:"confirm or lower: the recorded entry and the workflow's verdict after it"`
	Elisions     *Elisions                    `json:"elisions,omitempty"`
}

// delegationConfirmInputSchema is the tool's explicit input schema: the
// reflected one with the closed action enum and the lower-only tier enums
// applied. Any drift between the struct and these property names panics at
// registration rather than silently advertising an unconstrained field.
func delegationConfirmInputSchema() *jsonschema.Schema {
	s, err := jsonschema.For[delegationConfirmInput](nil)
	if err != nil {
		panic(fmt.Sprintf("fishhawk_delegation_confirm input schema: %v", err))
	}
	prop := func(parent *jsonschema.Schema, name string) *jsonschema.Schema {
		p, ok := parent.Properties[name]
		if !ok || p == nil {
			panic(fmt.Sprintf("fishhawk_delegation_confirm input schema: no property %q", name))
		}
		return p
	}
	actions := make([]any, 0, len(delegationconfirm.Actions()))
	for _, a := range delegationconfirm.Actions() {
		actions = append(actions, a)
	}
	prop(s, "action").Enum = actions
	prop(s, "proposed_tier").Enum = delegationLowerTiers
	prop(prop(s, "proposed_escalation"), "max_autonomy").Enum = delegationLowerTiers
	return s
}

// registerDelegationConfirm wires the fishhawk_delegation_confirm tool.
func registerDelegationConfirm(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_delegation_confirm",
		Description: strings.TrimSpace(`
WHEN: you are the incoming (or sitting) HUMAN captain of a repository and must
confirm or lower each workflow's delegation after a handover (ADR-083 rule 7),
or anyone wants to know which workflows are still unconfirmed.

ELIGIBILITY: action=read needs read:audit and writes nothing. confirm and lower
need write:approvals, must be made by the SITTING captain, and are REFUSED for
agent, run-bound and delegated identities (delegation_agent_identity_refused).

Actions (the set is closed; there is NO raise action):
  - read (default): every workflow of the delegation view at source/ref with
    its verdict — confirmed, unconfirmed (reason handover: nothing confirmed
    since the latest seat change; reason hash_stale: the delegation changed
    after it was confirmed) or no_captain — plus unconfirmed_workflows.
  - confirm (+workflow, +content_hash): record that the captain confirms the
    workflow's delegation EXACTLY as shown. Bind to the workflow's OWN
    content_hash; a hash that is no longer current is refused, so a later spec
    edit flips the workflow back to unconfirmed.
  - lower (+workflow, +reason, +proposed_tier and/or +proposed_escalation):
    file ONE autonomy:low work item carrying the proposed
    .fishhawk/workflows.yaml edit. It changes NO repository file and does not
    confirm: the delegation in force is unchanged until a human lands the
    edit. proposed_tier must be strictly below the current tier, and an
    escalation's max_autonomy at or below the effective proposed tier.

A confirmation counts only while its author is the captain in force: every
seat change resets confirmation state.

Tool errors: repo missing; unknown action; a field on the wrong action;
workflow missing on confirm/lower; validation_failed / delegation_raise_refused
/ delegation_nothing_proposed (400); authentication_required (401);
insufficient_scope / delegation_agent_identity_refused / delegation_not_captain
/ repo_forbidden (403); workflow_not_found / workflow_spec_not_found (404);
delegation_no_captain / delegation_hash_stale (409); workflow_spec_invalid
(422); forge_unavailable / work_item_filing_failed (502); github_unconfigured
(503); delegation_confirm_unconfigured (501).
`),
		InputSchema: delegationConfirmInputSchema(),
	}, resolver.delegationConfirm)
}

// delegationConfirm is the tool handler. Its local refusals run BEFORE any
// request, so a malformed call reaches no backend route.
func (r *runResolver) delegationConfirm(ctx context.Context, req *mcp.CallToolRequest, in delegationConfirmInput) (*mcp.CallToolResult, delegationConfirmOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		return nil, delegationConfirmOutput{}, fmt.Errorf("repo is required: pass repo or set GITHUB_REPOSITORY")
	}
	action := strings.TrimSpace(in.Action)
	if action == "" {
		action = delegationconfirm.ActionRead
	}
	if !isDelegationConfirmAction(action) {
		return nil, delegationConfirmOutput{}, fmt.Errorf("unknown action %q: one of %s (there is no raise action)",
			action, strings.Join(delegationconfirm.Actions(), ", "))
	}
	if err := checkDelegationConfirmFields(action, in); err != nil {
		return nil, delegationConfirmOutput{}, err
	}
	budget := r.responseBudget(req)
	if action == delegationconfirm.ActionRead {
		st, err := r.api.GetDelegationConfirmation(ctx, repo, in.Source, in.Ref)
		if err != nil {
			return nil, delegationConfirmOutput{}, fmt.Errorf("get delegation confirmation: %w", err)
		}
		out, err := boundDelegationConfirmOutput(delegationConfirmOutput{Confirmation: st}, budget)
		if err != nil {
			return nil, delegationConfirmOutput{}, err
		}
		return nil, out, nil
	}
	var res *delegationConfirmVerbResult
	var err error
	if action == delegationconfirm.ActionConfirm {
		res, err = r.api.PostDelegationConfirm(ctx, repo, delegationConfirmBody{
			Workflow: strings.TrimSpace(in.Workflow), ContentHash: strings.TrimSpace(in.ContentHash),
			Source: in.Source, Ref: in.Ref,
		})
	} else {
		res, err = r.api.PostDelegationLower(ctx, repo, delegationLowerBody{
			Workflow: strings.TrimSpace(in.Workflow), ProposedTier: strings.TrimSpace(in.ProposedTier),
			ProposedEscalation: in.ProposedEscalation, Reason: in.Reason, Source: in.Source, Ref: in.Ref,
			ParentEpic: in.ParentEpic, TitleVars: in.TitleVars, Labels: in.Labels,
		})
	}
	if err != nil {
		return nil, delegationConfirmOutput{}, fmt.Errorf("delegation %s: %w", action, err)
	}
	out, err := boundDelegationConfirmOutput(delegationConfirmOutput{Result: res}, budget)
	if err != nil {
		return nil, delegationConfirmOutput{}, err
	}
	return nil, out, nil
}

func isDelegationConfirmAction(action string) bool {
	for _, a := range delegationconfirm.Actions() {
		if a == action {
			return true
		}
	}
	return false
}

// checkDelegationConfirmFields refuses a field set on an action that does not
// take it, and a write action naming no workflow. A lower-only field on
// confirm is refused rather than ignored, so a caller never believes a
// proposal was recorded when only a confirmation was.
func checkDelegationConfirmFields(action string, in delegationConfirmInput) error {
	lowerOnly := map[string]bool{
		"proposed_tier":       strings.TrimSpace(in.ProposedTier) != "",
		"proposed_escalation": in.ProposedEscalation != nil,
		"reason":              strings.TrimSpace(in.Reason) != "",
		"parent_epic":         strings.TrimSpace(in.ParentEpic) != "",
		"title_vars":          len(in.TitleVars) > 0,
		"labels":              len(in.Labels) > 0,
	}
	confirmOnly := map[string]bool{"content_hash": strings.TrimSpace(in.ContentHash) != ""}
	refuse := func(set map[string]bool, only string) error {
		for _, name := range []string{"content_hash", "proposed_tier", "proposed_escalation", "reason", "parent_epic", "title_vars", "labels"} {
			if set[name] {
				return fmt.Errorf("%s applies only to action=%s; %s does not take it", name, only, action)
			}
		}
		return nil
	}
	switch action {
	case delegationconfirm.ActionRead:
		if strings.TrimSpace(in.Workflow) != "" {
			return fmt.Errorf("workflow applies only to action=confirm or lower; read returns every workflow")
		}
		if err := refuse(confirmOnly, delegationconfirm.ActionConfirm); err != nil {
			return err
		}
		return refuse(lowerOnly, delegationconfirm.ActionLower)
	case delegationconfirm.ActionConfirm:
		if err := refuse(lowerOnly, delegationconfirm.ActionLower); err != nil {
			return err
		}
	case delegationconfirm.ActionLower:
		if err := refuse(confirmOnly, delegationconfirm.ActionConfirm); err != nil {
			return err
		}
	}
	if strings.TrimSpace(in.Workflow) == "" {
		return fmt.Errorf("workflow is required for action=%s", action)
	}
	return nil
}

// delegationConfirmPath renders /v0/repos/{owner}/{name}/delegation/{leaf}.
func delegationConfirmPath(repo, leaf string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("repo must be owner/name, got %q", repo)
	}
	return "/v0/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/delegation/" + leaf, nil
}

// GetDelegationConfirmation calls GET
// /v0/repos/{owner}/{name}/delegation/confirmation (E76.5 / #3768): every
// workflow of the delegation view at source/ref with its confirmation
// verdict. Never writes.
func (c *apiClient) GetDelegationConfirmation(ctx context.Context, repo, source, ref string) (*delegationConfirmationState, error) {
	path, err := delegationConfirmPath(repo, "confirmation")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if source != "" {
		q.Set("source", source)
	}
	if ref != "" {
		q.Set("ref", ref)
	}
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var st delegationConfirmationState
	if err := c.do(ctx, http.MethodGet, path, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// delegationConfirmBody is the POST .../delegation/confirm body. It never
// sets `delegated`: a delegated confirmation is refused by the backend.
type delegationConfirmBody struct {
	Workflow    string `json:"workflow"`
	ContentHash string `json:"content_hash"`
	Source      string `json:"source,omitempty"`
	Ref         string `json:"ref,omitempty"`
}

// delegationLowerBody is the POST .../delegation/lower body.
type delegationLowerBody struct {
	Workflow           string                        `json:"workflow"`
	ProposedTier       string                        `json:"proposed_tier,omitempty"`
	ProposedEscalation *delegationconfirm.Escalation `json:"proposed_escalation,omitempty"`
	Reason             string                        `json:"reason"`
	Source             string                        `json:"source,omitempty"`
	Ref                string                        `json:"ref,omitempty"`
	ParentEpic         string                        `json:"parent_epic,omitempty"`
	TitleVars          map[string]string             `json:"title_vars,omitempty"`
	Labels             []string                      `json:"labels,omitempty"`
}

// PostDelegationConfirm calls POST .../delegation/confirm. A refusal
// appends nothing and surfaces as *apiError with the backend's code.
func (c *apiClient) PostDelegationConfirm(ctx context.Context, repo string, b delegationConfirmBody) (*delegationConfirmVerbResult, error) {
	return c.postDelegationVerb(ctx, repo, "confirm", b)
}

// PostDelegationLower calls POST .../delegation/lower. A refusal files
// nothing and appends nothing.
func (c *apiClient) PostDelegationLower(ctx context.Context, repo string, b delegationLowerBody) (*delegationConfirmVerbResult, error) {
	return c.postDelegationVerb(ctx, repo, "lower", b)
}

func (c *apiClient) postDelegationVerb(ctx context.Context, repo, leaf string, b any) (*delegationConfirmVerbResult, error) {
	path, err := delegationConfirmPath(repo, leaf)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal delegation %s: %w", leaf, err)
	}
	var res delegationConfirmVerbResult
	if err := c.do(ctx, http.MethodPost, path, body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// boundDelegationConfirmOutput is the ADR-077 ladder for
// fishhawk_delegation_confirm. Every tier MARKS its truncation, so the tool
// never returns a silently partial confirmation posture.
//
//	B1    drop each workflow's confirmation / lower_proposal DETAIL (and a
//	      verb event's payload), keeping every workflow's id, status, reason
//	      and current_content_hash — the verdict survives intact.
//	B2    read only: drop workflows from the TAIL (halving).
//	      unconfirmed_workflows is kept WHOLE, so the answer to "what is still
//	      unconfirmed?" is never cut here.
//	FLOOR every workflow entry dropped, unconfirmed_workflows halved until it
//	      fits, every retained string capped, under ONE aggregate elision.
func boundDelegationConfirmOutput(out delegationConfirmOutput, budget responseBudget) (delegationConfirmOutput, error) {
	set := func(o *delegationConfirmOutput, e *Elisions) { o.Elisions = e }
	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget.bytes {
		return out, nil
	}
	pointer := delegationConfirmSurface(out)
	led := &elisionLedger{budget: budget.bytes, source: budget.source, tier: "B1",
		note: "response reduced to fit the tool-result byte budget"}

	// B1: the per-workflow detail blocks and the event payload.
	details := 0
	strip := func(ws *delegationconfirm.WorkflowStatus) {
		if ws.Confirmation != nil || ws.LowerProposal != nil {
			ws.Confirmation, ws.LowerProposal = nil, nil
			details++
		}
	}
	if out.Confirmation != nil {
		st := *out.Confirmation
		st.Workflows = append([]delegationconfirm.WorkflowStatus(nil), st.Workflows...)
		for i := range st.Workflows {
			strip(&st.Workflows[i])
		}
		out.Confirmation = &st
	}
	if out.Result != nil {
		res := *out.Result
		strip(&res.Workflow)
		if res.Event.Payload != nil {
			res.Event.Payload = nil
			details++
		}
		out.Result = &res
	}
	if details > 0 {
		led.add(newStoredElision("workflows[].confirmation", fmt.Sprintf(
			"%d confirmation / lower-proposal detail blocks (and any event payload) were dropped; every workflow's status, reason and current_content_hash are kept, so the verdict is intact. The entries are on the audit chain and the surface below returns them",
			details), pointer.retrievalPointer, details))
	}
	fits, err := attachAndMeasureOut(&out, led, budget.bytes, set)
	if err != nil {
		return out, err
	}
	if fits {
		return out, nil
	}

	// B2: halve the read's workflow list from the tail.
	if out.Confirmation != nil {
		base := append([]elidedField(nil), led.entries...)
		total := len(out.Confirmation.Workflows)
		for len(out.Confirmation.Workflows) > 1 {
			keep := len(out.Confirmation.Workflows) / 2
			out.Confirmation.Workflows = out.Confirmation.Workflows[:keep]
			led.tier = "B2"
			led.entries = append(append([]elidedField(nil), base...), newStoredElision("workflows", fmt.Sprintf(
				"%d trailing workflows were dropped to fit the byte budget; unconfirmed_workflows is kept whole. The surface below returns every workflow",
				total-keep), pointer.retrievalPointer, total-keep))
			fits, ferr := attachAndMeasureOut(&out, led, budget.bytes, set)
			if ferr != nil {
				return out, ferr
			}
			if fits {
				return out, nil
			}
		}
	}
	return delegationConfirmFloor(out, pointer, budget)
}

// delegationConfirmSurface names the unbounded REST read every elision
// points at.
func delegationConfirmSurface(out delegationConfirmOutput) unboundedPointer {
	repo := "{owner}/{name}"
	if out.Confirmation != nil && out.Confirmation.Repo != "" {
		repo = out.Confirmation.Repo
	} else if out.Result != nil && out.Result.Repo != "" {
		repo = out.Result.Repo
	}
	return pointerREST("/v0/repos/" + repo + "/delegation/confirmation")
}

// delegationConfirmFloor is the constant-size floor. The fit is MEASURED: the
// retained unconfirmed list is halved until the floor fits, which converges
// because at zero ids the floor is a handful of capped scalars.
func delegationConfirmFloor(out delegationConfirmOutput, pointer unboundedPointer, budget responseBudget) (delegationConfirmOutput, error) {
	bound := max(budget.bytes, mcpConvergenceFloorBytes)
	keep := -1
	if out.Confirmation != nil {
		keep = len(out.Confirmation.UnconfirmedWorkflows)
	}
	for {
		floor, err := buildDelegationConfirmFloor(out, pointer, budget, keep)
		if err != nil {
			return out, err
		}
		n, err := marshalledLen(floor)
		if err != nil {
			return out, err
		}
		if n <= bound || keep <= 0 {
			return floor, nil
		}
		keep /= 2
	}
}

func capWorkflowStatus(ws delegationconfirm.WorkflowStatus) delegationconfirm.WorkflowStatus {
	return delegationconfirm.WorkflowStatus{
		Workflow:           capJSONString(ws.Workflow, floorFieldCap),
		Status:             capJSONString(ws.Status, floorFieldCap),
		Reason:             capJSONString(ws.Reason, floorFieldCap),
		CurrentContentHash: capJSONString(ws.CurrentContentHash, floorFieldCap),
	}
}

func buildDelegationConfirmFloor(out delegationConfirmOutput, pointer unboundedPointer, budget responseBudget, keep int) (delegationConfirmOutput, error) {
	floor := delegationConfirmOutput{}
	dropped := 0
	if out.Confirmation != nil {
		st := *out.Confirmation
		st.Repo = capJSONString(st.Repo, floorFieldCap)
		st.Source = capJSONString(st.Source, floorFieldCap)
		st.Ref = capJSONString(st.Ref, floorFieldCap)
		st.WorkflowSHA = capJSONString(st.WorkflowSHA, floorFieldCap)
		if st.Captain != nil {
			c := capJSONString(*st.Captain, floorFieldCap)
			st.Captain = &c
		}
		st.Workflows = []delegationconfirm.WorkflowStatus{}
		ids := make([]string, 0, keep)
		for i := 0; i < keep && i < len(out.Confirmation.UnconfirmedWorkflows); i++ {
			ids = append(ids, capJSONString(out.Confirmation.UnconfirmedWorkflows[i], floorFieldCap))
		}
		dropped = len(out.Confirmation.UnconfirmedWorkflows) - len(ids)
		st.UnconfirmedWorkflows = ids
		floor.Confirmation = &st
	}
	if out.Result != nil {
		res := *out.Result
		res.Repo = capJSONString(res.Repo, floorFieldCap)
		res.Workflow = capWorkflowStatus(res.Workflow)
		res.Event.Payload = nil
		res.Event.Category = capJSONString(res.Event.Category, floorFieldCap)
		res.Event.EntryHash = capJSONString(res.Event.EntryHash, floorFieldCap)
		if res.Filed != nil {
			f := delegationConfirmFiled{Number: res.Filed.Number, URL: capJSONString(res.Filed.URL, floorFieldCap),
				Title: capJSONString(res.Filed.Title, floorFieldCap)}
			res.Filed = &f
		}
		floor.Result = &res
	}
	led := &elisionLedger{budget: budget.bytes, source: budget.source, tier: floorTierName,
		note: "reduced to the constant-size floor: the captain, seat sequence and the unconfirmed workflow ids (or a verb's verdict and event identity) with every string capped. The entry below is an AGGREGATE — the floor tier's explicit exception to per-field itemisation"}
	led.add(aggregateStoredElision("*", fmt.Sprintf(
		"every per-workflow entry and event payload was omitted, %d of the unconfirmed workflow ids were dropped, and every retained string was capped to %d bytes; the surface below returns the full confirmation read",
		dropped, floorFieldCap), []string{pointer.String()}))
	w, err := led.wire()
	if err != nil {
		return out, err
	}
	floor.Elisions = w
	return floor, nil
}
