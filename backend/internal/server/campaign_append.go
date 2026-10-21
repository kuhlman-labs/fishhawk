package server

// campaign_append.go serves POST /v0/campaigns/{campaign_id}/items (E86.47 /
// #4358): append issues to an existing — possibly running — campaign, with the
// dependency closure re-validated under the campaign row lock and the change
// recorded on the audit chain BEFORE it commits.
//
// The core (appendCampaignItems) takes no ResponseWriter so the expedite verb
// (#4234) can reuse it; the handler owns only transport, scope and access.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// categoryCampaignItemsAppended is the audit category written, before commit,
// by every successful append. Registered in audit/categories.go.
const categoryCampaignItemsAppended = "campaign_items_appended"

// maxAppendCampaignItems bounds one append request: each new ref costs a forge
// read, so the bound keeps one call inside the issue-set resolution budget.
const maxAppendCampaignItems = 50

// appendCampaignItemsRequest is the POST /v0/campaigns/{campaign_id}/items
// body. Unknown fields are refused.
type appendCampaignItemsRequest struct {
	// Items are the issue refs to append (N, #N or issue:N), 1..50.
	Items []string `json:"items"`
	// AllowExternalBlockers admits a NEW item's depends_on edge onto an issue
	// that is not a campaign member: the edge is persisted and HOLDS the item
	// blocked until released. Without it such an edge refuses the append.
	AllowExternalBlockers bool `json:"allow_external_blockers,omitempty"`

	// requestStart anchors the issue-set resolution deadline (#3113) at the
	// caller's handler entry. Not part of the wire body.
	requestStart time.Time
}

// appendCampaignItemsResult is the core's successful output.
type appendCampaignItemsResult struct {
	Campaign *campaign.Campaign
	Appended []*campaign.Item
	// External maps an appended item's ref to its admitted non-member edges.
	External  map[string][]string
	Satisfied []workmgmt.SatisfiedEdge
}

// appendedCampaignItemResponse is one appended item in the 201 body: the
// standard item shape plus the admitted external blockers it carries.
type appendedCampaignItemResponse struct {
	campaignItemResponse
	ExternalDependsOn []string `json:"external_depends_on,omitempty"`
}

// handleAppendCampaignItems implements POST /v0/campaigns/{campaign_id}/items.
// Order: repo configured → write:campaigns scope → campaign_id → body →
// campaign exists → account ownership + repo visibility (BEFORE any forge
// read, terminal check or write) → the shared core.
func (s *Server) handleAppendCampaignItems(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()
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
	var req appendCampaignItemsRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}
	if len(req.Items) == 0 || len(req.Items) > maxAppendCampaignItems {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"items must name between 1 and "+strconv.Itoa(maxAppendCampaignItems)+" issue refs",
			map[string]any{"field": "items", "got": len(req.Items)})
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
	// Access BEFORE anything else: a caller from another account, or one that
	// cannot read the repo, must not drive a forge read or a write.
	if !s.enforceCampaignAccount(w, r, id) {
		return
	}
	if !s.enforceRepoVisibility(w, r, c.Repo) {
		return
	}
	req.requestStart = requestStart
	res, refusal := s.appendCampaignItems(r.Context(), c, req, true)
	if refusal != nil {
		s.writeCampaignSourceRefusal(w, r, refusal)
		return
	}
	appended := make([]appendedCampaignItemResponse, 0, len(res.Appended))
	for _, it := range res.Appended {
		appended = append(appended, appendedCampaignItemResponse{
			campaignItemResponse: toCampaignItemResponse(it),
			ExternalDependsOn:    res.External[it.IssueRef],
		})
	}
	body := map[string]any{
		"campaign": toCampaignResponse(res.Campaign),
		"appended": appended,
	}
	if sat := satisfiedDependencyPayloads(res.Satisfied); len(sat) > 0 {
		body["satisfied_dependencies"] = sat
	}
	s.writeJSON(w, r, http.StatusCreated, body)
}

// appendCampaignItems is the handler-independent append core. The caller has
// already authorized access to c. record=true binds the strict
// campaign_items_appended audit row as the repository's record-before-commit
// callback, so an unrecordable append writes nothing; a caller passing false
// takes on recording the change itself.
func (s *Server) appendCampaignItems(ctx context.Context, c *campaign.Campaign, req appendCampaignItemsRequest, record bool) (*appendCampaignItemsResult, *campaignSourceRefusal) {
	if req.requestStart.IsZero() {
		req.requestStart = time.Now()
	}
	if c.State.IsTerminal() {
		return nil, appendNotAppendable(c)
	}

	// Normalize every ref to issue:N (one parse, workmgmt.ParseIssueRef).
	newRefs := make([]string, 0, len(req.Items))
	for _, raw := range req.Items {
		n, err := workmgmt.ParseIssueRef(raw)
		if err != nil {
			return nil, &campaignSourceRefusal{
				Status:  http.StatusUnprocessableEntity,
				Code:    "campaign_item_ref_invalid",
				Message: fmt.Sprintf("item ref %q is not a valid issue reference: %v", raw, err),
				Details: map[string]any{"items": req.Items, "ref": raw},
			}
		}
		newRefs = append(newRefs, "issue:"+strconv.Itoa(n))
	}

	existing, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		return nil, &campaignSourceRefusal{
			Status: http.StatusInternalServerError, Code: "internal_error",
			Message: "list campaign items failed", Details: map[string]any{"error": err.Error()},
		}
	}
	// Duplicates are decided BEFORE the forge read: a re-append of a member
	// costs no tracker round-trip. The repository re-checks under the lock.
	if err := campaign.CheckDuplicates(existing, newRefs); err != nil {
		return nil, appendDuplicateRefusal(err)
	}

	// Resolve the NEW refs only, through the create path's resolver verbatim,
	// so existence/provider/installation/timeout/invalid-ref refusals match
	// start_campaign byte-for-byte. Existing members' tracker drift is never
	// read here (the dependency refresh observes it), so it cannot refuse an
	// unrelated append.
	srcReq := createCampaignRequest{Repo: c.Repo, Items: newRefs}
	epicRef, owner, name, refusal := validateCampaignSourceShape(srcReq)
	if refusal != nil {
		return nil, refusal
	}
	resolution, refusal := s.resolveCampaignSource(ctx, req.requestStart, srcReq, epicRef, owner, name)
	if refusal != nil {
		return nil, refusal
	}
	plan, err := campaign.PlanAppend(existing, newRefs, resolution.Result)
	if err != nil {
		if errors.Is(err, campaign.ErrDuplicateItem) {
			return nil, appendDuplicateRefusal(err)
		}
		return nil, &campaignSourceRefusal{
			Status: http.StatusInternalServerError, Code: "internal_error",
			Message: "plan campaign append failed", Details: map[string]any{"error": err.Error()},
		}
	}

	params := campaign.AppendCampaignItemsParams{
		CampaignID:    c.ID,
		NewItems:      plan.Items,
		AllowExternal: req.AllowExternalBlockers,
	}
	if record {
		params.Record = func(rctx context.Context, out campaign.AppendOutcome) error {
			appended := make([]map[string]any, 0, len(out.Created))
			for _, it := range out.Created {
				deps := it.DependsOn
				if deps == nil {
					deps = []string{}
				}
				ext := plan.External[it.IssueRef]
				if ext == nil {
					ext = []string{}
				}
				appended = append(appended, map[string]any{
					"issue_ref":  it.IssueRef,
					"depends_on": deps,
					"external":   ext,
					"autonomy":   it.Autonomy,
				})
			}
			return s.appendCampaignAuditStrict(rctx, categoryCampaignItemsAppended, map[string]any{
				"campaign_id":             c.ID.String(),
				"repo":                    c.Repo,
				"appended":                appended,
				"allow_external_blockers": req.AllowExternalBlockers,
				"satisfied_dependencies":  satisfiedDependencyPayloads(plan.Satisfied),
				"actor":                   IdentityFrom(rctx).Subject,
			})
		}
	}
	out, err := s.cfg.CampaignRepo.AppendCampaignItems(ctx, params)
	if err != nil {
		return nil, appendRepositoryRefusal(c, err)
	}

	// Re-derive: an appended eligible item lifts an awaiting_human campaign.
	if items, lerr := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(ctx, c.ID); lerr == nil {
		s.deriveCampaignAfterChange(ctx, c, items)
	}
	fresh := c
	if fc, ferr := s.cfg.CampaignRepo.GetCampaign(ctx, c.ID); ferr == nil {
		fresh = fc
	}
	return &appendCampaignItemsResult{
		Campaign:  fresh,
		Appended:  out.Created,
		External:  plan.External,
		Satisfied: plan.Satisfied,
	}, nil
}

func appendNotAppendable(c *campaign.Campaign) *campaignSourceRefusal {
	return &campaignSourceRefusal{
		Status:  http.StatusConflict,
		Code:    "campaign_not_appendable",
		Message: "campaign is " + string(c.State) + "; only a pending, running, paused or awaiting_human campaign accepts new items",
		Details: map[string]any{"campaign_id": c.ID.String(), "state": string(c.State)},
	}
}

func appendDuplicateRefusal(err error) *campaignSourceRefusal {
	var dup *campaign.DuplicateItemError
	var refs []string
	if errors.As(err, &dup) {
		refs = dup.Refs
	}
	return &campaignSourceRefusal{
		Status:  http.StatusConflict,
		Code:    "campaign_item_duplicate",
		Message: err.Error(),
		Details: map[string]any{"duplicates": refs},
	}
}

// appendRepositoryRefusal maps an AppendCampaignItems error onto the refusal
// the route answers. Every refusal here means NOTHING was written.
func appendRepositoryRefusal(c *campaign.Campaign, err error) *campaignSourceRefusal {
	var (
		ext   *campaign.ExternalDependencyError
		cycle *campaign.CycleError
		inv   campaign.InvalidTransitionError
	)
	switch {
	case errors.Is(err, campaign.ErrAppendUnrecorded):
		// Fail CLOSED: an unrecorded member could later be started.
		return &campaignSourceRefusal{
			Status:  http.StatusServiceUnavailable,
			Code:    "campaign_audit_unavailable",
			Message: "the append could not be recorded on the audit chain, so nothing was written; retry shortly",
			Details: map[string]any{internalCauseKey: err.Error()},
		}
	case errors.Is(err, campaign.ErrDuplicateItem):
		return appendDuplicateRefusal(err)
	case errors.As(err, &ext):
		return &campaignSourceRefusal{
			Status:  http.StatusUnprocessableEntity,
			Code:    "campaign_external_dependency",
			Message: err.Error(),
			Details: map[string]any{
				"edges":  ext.Edges,
				"remedy": "append the blocking issue in the same request, or pass allow_external_blockers to admit the edge (the item is then held blocked until the blocker is released)",
			},
		}
	case errors.As(err, &cycle):
		return &campaignSourceRefusal{
			Status:  http.StatusUnprocessableEntity,
			Code:    "campaign_dependency_cycle",
			Message: err.Error(),
			Details: map[string]any{"cycle": cycle.Cycle},
		}
	case errors.As(err, &inv):
		return &campaignSourceRefusal{
			Status:  http.StatusConflict,
			Code:    "campaign_not_appendable",
			Message: "campaign is " + inv.From + "; only a pending, running, paused or awaiting_human campaign accepts new items",
			Details: map[string]any{"campaign_id": c.ID.String(), "state": inv.From},
		}
	case errors.Is(err, campaign.ErrInvalidDependencyRef):
		return &campaignSourceRefusal{
			Status:  http.StatusUnprocessableEntity,
			Code:    "campaign_item_ref_invalid",
			Message: err.Error(),
			Details: map[string]any{"campaign_id": c.ID.String()},
		}
	case errors.Is(err, campaign.ErrCampaignNotFound):
		return &campaignSourceRefusal{
			Status: http.StatusNotFound, Code: "campaign_not_found",
			Message: "no campaign with that id", Details: map[string]any{"campaign_id": c.ID.String()},
		}
	default:
		return &campaignSourceRefusal{
			Status: http.StatusInternalServerError, Code: "internal_error",
			Message: "append campaign items failed", Details: map[string]any{"error": err.Error()},
		}
	}
}

// appendCampaignAuditStrict appends one campaign-level audit entry on the
// global chain and RETURNS its failure, unlike the best-effort
// emitCampaignAudit. It is the record-before-commit callback for a change that
// must not exist unrecorded: a nil AuditRepo is "cannot record", an error.
func (s *Server) appendCampaignAuditStrict(ctx context.Context, category string, payload map[string]any) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("audit repository is not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", category, err)
	}
	kind := audit.ActorUser
	subject := IdentityFrom(ctx).Subject
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      body,
		AccountID:    identityAccountID(ctx),
	}); err != nil {
		return fmt.Errorf("append %s: %w", category, err)
	}
	return nil
}
