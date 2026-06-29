package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Campaign list pagination bounds, mirroring runsDefaultLimit / runsMaxLimit.
const (
	campaignsDefaultLimit = 50
	campaignsMaxLimit     = 200
)

// campaignResponse is the JSON shape POST /v0/campaigns and
// GET /v0/campaigns/{id} return. Field names + types match
// docs/api/v0.openapi.yaml's `Campaign` schema exactly, mirroring
// runResponse (runs.go) so there's never a translation step between the
// OpenAPI doc and the wire format.
type campaignResponse struct {
	ID      uuid.UUID `json:"id"`
	Repo    string    `json:"repo"`
	EpicRef string    `json:"epic_ref"`
	State   string    `json:"state"`
	// PausePolicy is the operator-chosen pause behavior on a gate hand-off
	// (E25.7): pause_campaign (block the whole campaign, the default) or
	// pause_item (continue-others). Always normalized (never empty) on a
	// persisted campaign.
	PausePolicy string `json:"pause_policy"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent delegation
	// override (E25.12 / #1451): when present it wins WHOLESALE as the
	// outermost rung of the resolution ladder (campaign > gate > workflow) for
	// every issue-run of the campaign. Surfaced as the raw JSON block the
	// campaign was created with (omitted when the campaign carries no override,
	// the unchanged-behavior default). Because it wins wholesale for every
	// issue-run, the campaign block IS each issue-run's effective contract when
	// present.
	OperatorAgent json.RawMessage `json:"operator_agent,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// campaignItemResponse mirrors docs/api/v0.openapi.yaml's `CampaignItem`
// schema. run_id is omitempty (a *uuid.UUID) so an unlinked item — the
// pre-dispatch default — carries no run_id key rather than a null.
type campaignItemResponse struct {
	ID        uuid.UUID  `json:"id"`
	IssueRef  string     `json:"issue_ref"`
	DependsOn []string   `json:"depends_on"`
	RunID     *uuid.UUID `json:"run_id,omitempty"`
	State     string     `json:"state"`
	// PauseReason records why a paused item was handed off to a human (the
	// page event + run/stage/gate). Omitted (omitempty) unless the item is —
	// or was — paused; mirrors the domain *PauseReason.
	PauseReason *campaign.PauseReason `json:"pause_reason,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
}

// campaignRollupPayload is the engine's readiness partition over a
// campaign's items (campaign.Eligibility, engine.go). Every slice holds
// issue refs; an item appears in exactly one slice. Mirrors
// docs/api/v0.openapi.yaml's `CampaignRollup` schema. Slices are
// normalized to non-nil so the wire shape is always a JSON array.
type campaignRollupPayload struct {
	Eligible  []string `json:"eligible"`
	Blocked   []string `json:"blocked"`
	Running   []string `json:"running"`
	Done      []string `json:"done"`
	Failed    []string `json:"failed"`
	Cancelled []string `json:"cancelled"`
	// Paused holds items the auto-driver handed off to a human (E25.7). Like
	// the other slices it is always an array (never null).
	Paused []string `json:"paused"`
}

// campaignNextActionPayload tells the operator-agent what to do next with
// a campaign, distilled from the rollup partition by
// computeCampaignNextAction. Mirrors runNextActionPayload (runs.go): kept
// distinct from any domain type so the API surface is explicit.
type campaignNextActionPayload struct {
	Action   string `json:"action"`
	IssueRef string `json:"issue_ref,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// createCampaignRequest is the POST /v0/campaigns body: the repo and the
// epic ref to decompose into the campaign DAG. Both required.
type createCampaignRequest struct {
	Repo    string `json:"repo"`
	EpicRef string `json:"epic_ref"`
	// PausePolicy is the OPTIONAL pause behavior for a gate hand-off (E25.7):
	// "pause_campaign" (block the whole campaign, the default) or "pause_item"
	// (continue-others). Empty normalizes to pause_campaign inside
	// campaign.Persist, so omitting it yields the conservative default.
	PausePolicy string `json:"pause_policy,omitempty"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent override
	// (E25.12 / #1451): a delegation block that tightens or relaxes the
	// per-workflow contract for ALL the campaign's issue-runs, winning
	// wholesale over the gate/workflow blocks. Carried as raw JSON and
	// validated against spec.OperatorAgent (unknown fields rejected) in the
	// handler before it is stored opaquely. Omit it for no override (each
	// issue-run inherits its workflow's contract — the unchanged default).
	OperatorAgent json.RawMessage `json:"operator_agent,omitempty"`
}

func toCampaignResponse(c *campaign.Campaign) campaignResponse {
	return campaignResponse{
		ID:          c.ID,
		Repo:        c.Repo,
		EpicRef:     c.EpicRef,
		State:       string(c.State),
		PausePolicy: string(c.PausePolicy),
		// Raw JSON passthrough: nil bytes → omitted (omitempty), so a campaign
		// with no override carries no operator_agent key.
		OperatorAgent: json.RawMessage(c.OperatorAgent),
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
}

func toCampaignItemResponse(it *campaign.Item) campaignItemResponse {
	deps := it.DependsOn
	if deps == nil {
		deps = []string{}
	}
	return campaignItemResponse{
		ID:          it.ID,
		IssueRef:    it.IssueRef,
		DependsOn:   deps,
		RunID:       it.RunID,
		State:       string(it.State),
		PauseReason: it.PauseReason,
		CreatedAt:   it.CreatedAt,
		UpdatedAt:   it.UpdatedAt,
	}
}

// toCampaignRollupPayload maps a campaign.Eligibility partition onto the
// wire payload, normalizing every nil slice to an empty slice so the JSON
// shape is a stable array (never null) for each partition.
func toCampaignRollupPayload(e campaign.Eligibility) campaignRollupPayload {
	nz := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return campaignRollupPayload{
		Eligible:  nz(e.Eligible),
		Blocked:   nz(e.Blocked),
		Running:   nz(e.Running),
		Done:      nz(e.Done),
		Failed:    nz(e.Failed),
		Cancelled: nz(e.Cancelled),
		Paused:    nz(e.Paused),
	}
}

// computeCampaignNextAction distills the rollup partition into the single
// next step for the operator-agent. PRECEDENCE is FAILED-wins and STRICT,
// in this order:
//
//  1. any failed item -> "attention" on the first failed ref. A terminal
//     item failure forces operator attention (retry/abandon) REGARDLESS of
//     whether eligible items also exist — this check is FIRST and
//     unconditional, with NO "start_run whenever eligible is non-empty"
//     short-circuit ahead of it.
//  2. else any paused item -> "resume" on the first paused ref. The
//     auto-driver handed a gate off to a human (E25.7); the campaign is
//     stalled until the gate is handled and the operator resumes it. Ranked
//     above start_run so a paused hand-off is surfaced before new dispatch.
//  3. else any eligible item -> "start_run" on the first eligible ref.
//  4. else any running or blocked item -> "wait".
//  5. else (every item terminal done/cancelled) -> "complete".
func computeCampaignNextAction(e campaign.Eligibility) campaignNextActionPayload {
	switch {
	case len(e.Failed) > 0:
		return campaignNextActionPayload{
			Action:   "attention",
			IssueRef: e.Failed[0],
			Detail:   "a campaign item failed; retry or abandon it before the campaign can proceed",
		}
	case len(e.Paused) > 0:
		return campaignNextActionPayload{
			Action:   "resume",
			IssueRef: e.Paused[0],
			Detail:   "the auto-driver paged a human at a run gate; handle the gate then POST /resume to continue the campaign",
		}
	case len(e.Eligible) > 0:
		return campaignNextActionPayload{
			Action:   "start_run",
			IssueRef: e.Eligible[0],
			Detail:   "this item's dependencies are satisfied and it has no run yet",
		}
	case len(e.Running) > 0 || len(e.Blocked) > 0:
		return campaignNextActionPayload{
			Action: "wait",
			Detail: "items are running or blocked on a dependency; nothing to dispatch yet",
		}
	default:
		return campaignNextActionPayload{
			Action: "complete",
			Detail: "every campaign item reached a terminal state",
		}
	}
}

// handleCreateCampaign implements POST /v0/campaigns. It assembles a
// campaign from an epic ref — querying the epic's children + depends_on
// edges via the workmgmt provider, wave-ordering them with
// campaign.Assemble, and persisting the result with campaign.Persist — and
// returns the created campaign.
//
// This is a RUNLESS create: there is no run row, so it resolves the repo's
// GitHub App installation directly via s.cfg.GitHub.GetRepoInstallation
// (the same path POST /v0/runs uses at runs.go:498), which the real GitHub
// provider needs to mint an installation token for the children query.
//
// There is deliberately NO Idempotency-Key handling: the campaigns table
// has no idempotency_key column and campaign.Repository exposes no
// dedup lookup, so a header cannot be honoured here — see the OpenAPI note
// and the PR Notes follow-up.
func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}

	var req createCampaignRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	owner, name, ok := splitRepoFullName(req.Repo)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be in owner/name form",
			map[string]any{"field": "repo", "got": req.Repo})
		return
	}
	if strings.TrimSpace(req.EpicRef) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"epic_ref is required", map[string]any{"field": "epic_ref"})
		return
	}
	// pause_policy is optional; an empty value normalizes to pause_campaign in
	// campaign.Persist. A non-empty value must be a recognized policy, caught
	// here so a typo surfaces a 400 rather than a DB CHECK violation at insert.
	if req.PausePolicy != "" && !validPausePolicies[req.PausePolicy] {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"pause_policy must be one of pause_campaign, pause_item",
			map[string]any{"field": "pause_policy", "got": req.PausePolicy})
		return
	}
	// operator_agent is OPTIONAL. When present, it must be a well-formed
	// spec.OperatorAgent block (unknown fields rejected) — validated HERE so a
	// malformed override surfaces a 400 at create rather than being stored
	// opaquely and failing later at the auto-driver consumer (slice B). The
	// validated raw bytes are stored verbatim; the campaign package stays
	// spec-free. A JSON `null` (or omission) is treated as "no override".
	operatorAgentBytes, ok := validateOperatorAgent(req.OperatorAgent)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"operator_agent must be a valid operator-agent block",
			map[string]any{"field": "operator_agent"})
		return
	}

	// Resolve the App installation for the target repo (#713 / runs.go:498).
	// A runless create has no run row to carry the id, so resolve it directly:
	// the real GitHub provider needs it to query the epic's children.
	var instID int64
	if s.cfg.GitHub != nil {
		id, err := s.cfg.GitHub.GetRepoInstallation(r.Context(), githubclient.RepoRef{Owner: owner, Name: name})
		switch {
		case err == nil:
			instID = id
		case errors.Is(err, githubclient.ErrNotInstalled):
			s.writeError(w, r, http.StatusUnprocessableEntity, "repo_not_installed",
				"GitHub App is not installed on the target repository",
				map[string]any{"repo": req.Repo})
			return
		default:
			s.writeError(w, r, http.StatusBadGateway, "installation_resolution_failed",
				"could not resolve the GitHub App installation for the target repo",
				map[string]any{"error": err.Error()})
			return
		}
	}

	// Resolve the work-management provider and its optional epic-children
	// query capability, exactly as POST /v0/work-items does (workitems.go).
	conv, err := conventionsLoader(req.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		var unk *workmgmt.UnknownProviderError
		if errors.As(err, &unk) {
			s.writeError(w, r, http.StatusNotImplemented, "provider_unimplemented",
				unk.Error(), map[string]any{"provider": unk.ID, "registered": unk.Known})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve work-item provider", map[string]any{"error": err.Error()})
		return
	}
	querier, ok := provider.(workmgmt.EpicChildrenQuerier)
	if !ok {
		s.writeError(w, r, http.StatusNotImplemented, "epic_children_unsupported",
			"the configured work-item provider cannot query epic children",
			map[string]any{"provider": conv.Provider})
		return
	}

	result, err := querier.EpicChildren(r.Context(), workmgmt.EpicChildrenRequest{
		Target: workmgmt.Target{
			Repo:           workmgmt.Repo{Owner: owner, Name: name},
			InstallationID: instID,
			Project:        conv.Project,
			Jira:           conv.Jira,
		},
		Epic: req.EpicRef,
	})
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "epic_children_query_failed",
			"could not query the epic's children",
			map[string]any{"error": err.Error()})
		return
	}

	// Assemble the wave-ordered DAG, failing closed on a dangling dependency
	// (a depends_on target that is not a fellow child) or a dependency cycle.
	assembly, err := campaign.Assemble(req.EpicRef, result)
	if err != nil {
		switch {
		case errors.Is(err, campaign.ErrDanglingDependency):
			s.writeError(w, r, http.StatusUnprocessableEntity, "campaign_dangling_dependency",
				err.Error(), map[string]any{"epic_ref": req.EpicRef})
			return
		case errors.Is(err, campaign.ErrCycle):
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(), map[string]any{"epic_ref": req.EpicRef})
			return
		default:
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(), map[string]any{"epic_ref": req.EpicRef})
			return
		}
	}

	// Thread the create request's pause_policy onto the assembly so it reaches
	// the persisted campaign (the call-site update deferred from slice 1; a
	// zero value is normalized to pause_campaign inside campaign.Persist).
	assembly.PausePolicy = campaign.PausePolicy(req.PausePolicy)
	// Thread the validated campaign-level operator_agent override onto the
	// assembly (E25.12). Nil = no override; the campaign inherits each
	// issue-run's workflow contract unchanged.
	assembly.OperatorAgent = operatorAgentBytes

	created, err := campaign.Persist(r.Context(), s.cfg.CampaignRepo, req.Repo, assembly)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"persist campaign failed", map[string]any{"error": err.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusCreated, toCampaignResponse(created))
}

// handleListCampaigns implements GET /v0/campaigns. Offset-cursor
// paginated, optional repo + state filters, mirroring handleListRuns.
func (s *Server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"), campaignsDefaultLimit, campaignsMaxLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return
	}
	offset, err := decodeOffsetCursor(q.Get("cursor"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "cursor_invalid", err.Error(), nil)
		return
	}
	stateFilter := q.Get("state")
	if stateFilter != "" {
		if _, ok := validCampaignStates[stateFilter]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"state must be one of pending, running, paused, succeeded, failed, cancelled",
				map[string]any{"field": "state", "got": stateFilter})
			return
		}
	}

	// Fetch one extra row to compute the next cursor without a COUNT, the
	// same limit+1 trick handleListRuns uses.
	rows, err := s.cfg.CampaignRepo.ListCampaigns(r.Context(), campaign.ListCampaignsFilter{
		Repo:   q.Get("repo"),
		State:  stateFilter,
		Limit:  limit + 1,
		Offset: offset,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaigns failed", map[string]any{"error": err.Error()})
		return
	}

	var nextCursor string
	if len(rows) > limit {
		nextCursor = encodeOffsetCursor(offset + limit)
		rows = rows[:limit]
	}
	items := make([]campaignResponse, 0, len(rows))
	for _, c := range rows {
		items = append(items, toCampaignResponse(c))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

// validCampaignStates pins the closed set per docs/api/v0.openapi.yaml,
// defense-in-depth at the handler so a typo'd state filter surfaces a 400
// rather than reaching the DB layer.
var validCampaignStates = map[string]struct{}{
	string(campaign.StatePending):   {},
	string(campaign.StateRunning):   {},
	string(campaign.StatePaused):    {},
	string(campaign.StateSucceeded): {},
	string(campaign.StateFailed):    {},
	string(campaign.StateCancelled): {},
}

// validPausePolicies pins the closed set of create-request pause_policy values
// per docs/api/v0.openapi.yaml, defense-in-depth at the handler so a typo
// surfaces a 400 rather than reaching the column CHECK at insert. The empty
// value is intentionally NOT here — it is the "omit, take the default" signal
// normalized to pause_campaign inside campaign.Persist.
var validPausePolicies = map[string]bool{
	string(campaign.PausePolicyPauseCampaign): true,
	string(campaign.PausePolicyPauseItem):     true,
}

// validateOperatorAgent validates the OPTIONAL campaign-level operator_agent
// override carried as raw JSON. It returns the bytes to store plus ok:
//   - empty/absent (len 0) or a JSON `null` literal -> (nil, true): no override
//     (each issue-run inherits its workflow contract — the unchanged default).
//   - a well-formed spec.OperatorAgent block (unknown fields rejected) ->
//     (raw, true): stored verbatim, opaque to the campaign package.
//   - anything else — malformed JSON, an unknown field, or a non-object —
//     -> (nil, false): the handler answers 400 validation_failed.
//
// Validating against the spec.OperatorAgent Go type with DisallowUnknownFields
// IS the v0 validation (no separate JSON schema), consistent with how
// pause_policy is validated in Go. The reused spec type is why no
// docs/spec/*.schema.json change — and so no schema-sync gate — is triggered.
func validateOperatorAgent(raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var oa spec.OperatorAgent
	if err := dec.Decode(&oa); err != nil {
		return nil, false
	}
	return raw, true
}

// handleGetCampaign implements GET /v0/campaigns/{campaign_id}.
func (s *Server) handleGetCampaign(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}
	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, toCampaignResponse(c))
}

// handleListCampaignItems implements GET /v0/campaigns/{campaign_id}/items.
func (s *Server) handleListCampaignItems(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}
	// ListCampaignItemsForCampaign returns an empty slice (not ErrNotFound)
	// for an unknown campaign id, so existence is checked explicitly via
	// GetCampaign first — a 404 for a missing campaign is more useful than an
	// ambiguous empty list.
	if _, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id); err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}
	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}
	out := make([]campaignItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, toCampaignItemResponse(it))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"items": out})
}

// handleGetCampaignStatus implements GET /v0/campaigns/{campaign_id}/status:
// the campaign + its items + the engine's readiness rollup + the distilled
// next_action. This is the surface the operator-agent polls to drive a
// campaign.
func (s *Server) handleGetCampaignStatus(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}
	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}
	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}

	elig := campaign.NextEligible(items)
	out := make([]campaignItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, toCampaignItemResponse(it))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"campaign":    toCampaignResponse(c),
		"items":       out,
		"rollup":      toCampaignRollupPayload(elig),
		"next_action": computeCampaignNextAction(elig),
	})
}

// handleResumeCampaign implements POST /v0/campaigns/{campaign_id}/resume: the
// operator's hand-back after the auto-driver paged a human at a run gate
// (E25.7 / ADR-047 Track C). It flips the campaign paused → running (when the
// campaign itself was paused — the pause_campaign policy) AND every paused item
// paused → running, so the next driver tick re-engages the campaign and the
// continuation proceeds.
//
// It serves BOTH pause policies: under pause_campaign the campaign and the
// affected item are paused together; under pause_item only the item is paused
// while the campaign stays running. So "nothing to resume" is the joint
// condition — the campaign is not paused AND no item is paused — which yields
// 409 campaign_not_paused; a resume with any paused item proceeds even when the
// campaign is already running.
//
// The nil-CampaignRepo guard is checked BEFORE the write-scope check so an
// unconfigured deployment answers 503 (not 401), matching the read handlers and
// the 503-vs-404 route-registration idiom.
func (s *Server) handleResumeCampaign(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}

	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}

	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}
	var pausedItems []*campaign.Item
	for _, it := range items {
		if it.State == campaign.ItemStatePaused {
			pausedItems = append(pausedItems, it)
		}
	}

	// Nothing is paused on either axis — there is nothing to resume.
	if c.State != campaign.StatePaused && len(pausedItems) == 0 {
		s.writeError(w, r, http.StatusConflict, "campaign_not_paused",
			"campaign has no paused state to resume",
			map[string]any{"campaign_id": id.String(), "state": string(c.State)})
		return
	}

	// Resume the campaign first (pause_campaign policy). A same-state running
	// campaign (pause_item policy) is left untouched — only paused items move.
	if c.State == campaign.StatePaused {
		if _, err := s.cfg.CampaignRepo.TransitionCampaign(r.Context(), id, campaign.StateRunning); err != nil {
			var inv campaign.InvalidTransitionError
			if errors.As(err, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_transition",
					err.Error(), map[string]any{"campaign_id": id.String()})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resume campaign failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// Resume each paused item. Best-effort retryability: a transition error
	// surfaces 500, and because a retry re-enters with the still-paused items
	// (the joint condition above), it is not stranded by the campaign already
	// being running.
	for _, it := range pausedItems {
		if _, err := s.cfg.CampaignRepo.TransitionCampaignItem(r.Context(), it.ID, campaign.ItemStateRunning); err != nil {
			var inv campaign.InvalidTransitionError
			if errors.As(err, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_transition",
					err.Error(), map[string]any{"item_id": it.ID.String()})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resume campaign item failed",
				map[string]any{"error": err.Error(), "item_id": it.ID.String()})
			return
		}
	}

	// Return the freshest campaign state.
	updated, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign after resume failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, toCampaignResponse(updated))
}
