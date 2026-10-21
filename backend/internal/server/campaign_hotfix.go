package server

// campaign_hotfix.go serves POST /v0/campaigns/{campaign_id}/hotfix (E83.100 /
// #4234): add ONE urgent issue to a non-terminal campaign as an EXPEDITED item
// at the head of its queue, run under its recorded workflow, optionally
// placing a campaign DISPATCH HOLD that the host-dispatch marker enforces.
//
// The verb reuses the #4358 append core (appendCampaignItems) with its own
// record-before-commit recorder, so the item, its expedite row and the hold
// commit only together with their campaign_item_added audit row. It also owns
// releaseCampaignHoldIfDue, the one hold-release path the verb, the status
// read and the host-dispatch marker share.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// The audit categories the hotfix verb and the hold release write. Both are
// registered in audit/categories.go.
const (
	// categoryCampaignItemAdded: one expedited item added by the hotfix verb,
	// recorded before commit.
	categoryCampaignItemAdded = "campaign_item_added"
	// categoryCampaignHoldReleased: a dispatch hold released, recorded before
	// commit under the system actor.
	categoryCampaignHoldReleased = "campaign_hold_released"
)

// defaultHotfixWorkflow is the workflow an expedited item runs under when the
// request names none. It needs no reason; every other workflow does.
const defaultHotfixWorkflow = "hotfix_change"

// maxHotfixReasonBytes caps the operator's free-text reason.
const maxHotfixReasonBytes = 1000

// campaignHoldSystemSubject is the actor subject of a campaign_hold_released
// row: a release is a system decision whatever request triggered it.
const campaignHoldSystemSubject = "system:campaign-hold-release"

// The evaluation points that can release a due hold, recorded as the
// campaign_hold_released row's trigger.
const (
	holdReleaseTriggerHotfixVerb   = "hotfix_verb"
	holdReleaseTriggerStatusRead   = "status_read"
	holdReleaseTriggerHostDispatch = "host_dispatch"
)

// campaignHotfixRequest is the POST /v0/campaigns/{campaign_id}/hotfix body.
// Unknown fields are refused.
type campaignHotfixRequest struct {
	// Issue is the issue ref to add (N, #N or issue:N).
	Issue string `json:"issue"`
	// Workflow is the workflow the item runs under; default hotfix_change.
	Workflow string `json:"workflow,omitempty"`
	// Reason is the operator's justification, required for any workflow other
	// than hotfix_change. Untrusted-origin text: persisted and rendered only as
	// data, never copied into an agent prompt or a promote reason.
	Reason string `json:"reason,omitempty"`
	// HoldDispatch places a campaign dispatch hold until the hotfix is terminal
	// or campaign.MaxDispatchHold elapses.
	HoldDispatch bool `json:"hold_dispatch,omitempty"`
}

// dispatchHoldPayload is the wire shape of an active dispatch hold, in the
// hotfix 201 body and the status read's dispatch_hold block.
type dispatchHoldPayload struct {
	HotfixItemID   uuid.UUID `json:"hotfix_item_id"`
	HotfixIssueRef string    `json:"hotfix_issue_ref"`
	HeldAt         time.Time `json:"held_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func toDispatchHoldPayload(h *campaign.DispatchHold, issueRef string) *dispatchHoldPayload {
	if h == nil {
		return nil
	}
	return &dispatchHoldPayload{
		HotfixItemID:   h.HotfixItemID,
		HotfixIssueRef: issueRef,
		HeldAt:         h.HeldAt,
		ExpiresAt:      h.ExpiresAt,
	}
}

// handleCampaignHotfix implements POST /v0/campaigns/{campaign_id}/hotfix.
// Every refusal writes nothing. Order: repo configured → write:campaigns →
// operator-only (run-bound and delegated operator-agent tokens refused,
// unconditionally) → campaign_id → strict body → issue → workflow default →
// reason cap → reason rule → campaign exists → account + repo visibility →
// terminal → issue ref + duplicate (before any forge read) → workflow
// registration → release a due hold → the shared append core.
func (s *Server) handleCampaignHotfix(w http.ResponseWriter, r *http.Request) {
	requestStart := time.Now()
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	ident := IdentityFrom(r.Context())
	if _, runBound := runBoundTokenRunID(ident); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not add a campaign hotfix; expediting an item and holding a campaign's dispatches is an operator action", nil)
		return
	}
	if operatorrole.IsTokenSubject(ident.Subject) {
		s.writeError(w, r, http.StatusForbidden, "operator_agent_forbidden",
			"a delegated operator-agent token may not add a campaign hotfix; expediting an item and holding a campaign's dispatches is an operator action",
			map[string]any{"subject": ident.Subject})
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}
	var req campaignHotfixRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Issue) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"issue is required", map[string]any{"field": "issue"})
		return
	}
	workflow := strings.TrimSpace(req.Workflow)
	if workflow == "" {
		workflow = defaultHotfixWorkflow
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) > maxHotfixReasonBytes {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason must be at most "+strconv.Itoa(maxHotfixReasonBytes)+" bytes",
			map[string]any{"field": "reason", "max_bytes": maxHotfixReasonBytes, "got_bytes": len(reason)})
		return
	}
	if workflow != defaultHotfixWorkflow && reason == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"a hotfix run under any workflow other than "+defaultHotfixWorkflow+" needs a non-empty reason",
			map[string]any{"field": "reason", "workflow": workflow})
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
	if !s.enforceCampaignAccount(w, r, id) {
		return
	}
	if !s.enforceRepoVisibility(w, r, c.Repo) {
		return
	}
	if c.State.IsTerminal() {
		s.writeCampaignSourceRefusal(w, r, appendNotAppendable(c))
		return
	}
	n, err := workmgmt.ParseIssueRef(req.Issue)
	if err != nil {
		s.writeCampaignSourceRefusal(w, r, &campaignSourceRefusal{
			Status:  http.StatusUnprocessableEntity,
			Code:    "campaign_item_ref_invalid",
			Message: fmt.Sprintf("issue ref %q is not a valid issue reference: %v", req.Issue, err),
			Details: map[string]any{"issue": req.Issue},
		})
		return
	}
	issueRef := "issue:" + strconv.Itoa(n)
	// Duplicates are decided BEFORE the workflow-spec read, so a re-add of a
	// member costs no forge round-trip. The core and the repository re-check.
	existing, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}
	if err := campaign.CheckDuplicates(existing, []string{issueRef}); err != nil {
		s.writeCampaignSourceRefusal(w, r, appendDuplicateRefusal(err))
		return
	}
	if refusal := s.hotfixWorkflowRegistered(r.Context(), c, workflow); refusal != nil {
		s.writeCampaignSourceRefusal(w, r, refusal)
		return
	}
	// A due hold is released first, so a new hold_dispatch is not refused by a
	// hold that has already lapsed or whose hotfix is terminal.
	if held, err := s.releaseCampaignHoldIfDue(r.Context(), c, holdReleaseTriggerHotfixVerb); err != nil {
		s.writeCampaignSourceRefusal(w, r, holdReleaseRefusal(c, held, err))
		return
	}

	exp := &campaign.ExpediteParams{Kind: campaign.ExpediteKindHotfix, Workflow: workflow, Reason: reason}
	if req.HoldDispatch {
		now := s.nowFunc().UTC()
		exp.Hold = &campaign.DispatchHoldParams{HeldAt: now, ExpiresAt: now.Add(campaign.MaxDispatchHold)}
	}
	res, refusal := s.appendCampaignItems(r.Context(), c, appendCampaignItemsRequest{
		Items:        []string{issueRef},
		requestStart: requestStart,
		expedite:     exp,
	}, s.campaignItemAddedRecorder(c, exp, req.HoldDispatch))
	if refusal != nil {
		s.writeCampaignSourceRefusal(w, r, refusal)
		return
	}
	if len(res.Appended) != 1 {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"hotfix append returned no item", map[string]any{"appended": len(res.Appended)})
		return
	}
	item := res.Appended[0]
	body := map[string]any{
		"campaign": toCampaignResponse(res.Campaign),
		"item":     toCampaignItemResponse(item),
	}
	if res.Hold != nil {
		body["dispatch_hold"] = toDispatchHoldPayload(res.Hold, item.IssueRef)
	}
	s.writeJSON(w, r, http.StatusCreated, body)
}

// campaignItemAddedRecorder is the hotfix verb's record-before-commit
// callback: exactly one strict campaign_item_added row on the global chain.
func (s *Server) campaignItemAddedRecorder(c *campaign.Campaign, exp *campaign.ExpediteParams, holdDispatch bool) appendRecorder {
	return func(rctx context.Context, out campaign.AppendOutcome, _ *campaign.AppendPlan) error {
		if len(out.Created) != 1 {
			return fmt.Errorf("expedited append created %d items, want 1", len(out.Created))
		}
		it := out.Created[0]
		deps := it.DependsOn
		if deps == nil {
			deps = []string{}
		}
		payload := map[string]any{
			"campaign_id":   c.ID.String(),
			"repo":          c.Repo,
			"item_id":       it.ID.String(),
			"issue_ref":     it.IssueRef,
			"expedited":     true,
			"kind":          exp.Kind,
			"workflow":      exp.Workflow,
			"reason":        exp.Reason,
			"position":      it.Position,
			"depends_on":    deps,
			"hold_dispatch": holdDispatch,
			"actor":         IdentityFrom(rctx).Subject,
		}
		if out.Hold != nil {
			payload["hold_held_at"] = out.Hold.HeldAt
			payload["hold_expires_at"] = out.Hold.ExpiresAt
		}
		return s.appendCampaignAuditStrict(rctx, categoryCampaignItemAdded, payload)
	}
}

// hotfixWorkflowRegistered is the FAIL-CLOSED workflow-registration gate: the
// requested workflow must be declared by the repository's spec on its default
// branch. Never a silent fallback — an undeclared workflow refuses 422
// workflow_not_registered listing the declared ones.
func (s *Server) hotfixWorkflowRegistered(ctx context.Context, c *campaign.Campaign, workflow string) *campaignSourceRefusal {
	if s.cfg.GitHub == nil {
		return &campaignSourceRefusal{
			Status:  http.StatusServiceUnavailable,
			Code:    "workflow_spec_unavailable",
			Message: "no GitHub client is configured, so the workflow cannot be checked against the repository spec",
			Details: map[string]any{"repo": c.Repo, "requested": workflow},
		}
	}
	owner, name, ok := strings.Cut(c.Repo, "/")
	if !ok || owner == "" || name == "" {
		return &campaignSourceRefusal{
			Status:  http.StatusInternalServerError,
			Code:    "internal_error",
			Message: "campaign repo is not owner/name",
			Details: map[string]any{"repo": c.Repo},
		}
	}
	repoRef := forge.RepoRef{Owner: owner, Name: name}
	unavailable := func(err error) *campaignSourceRefusal {
		return &campaignSourceRefusal{
			Status:  http.StatusBadGateway,
			Code:    "workflow_spec_unavailable",
			Message: "fetching the repository workflow spec failed; retry shortly",
			Details: map[string]any{"repo": c.Repo, "requested": workflow, internalCauseKey: err.Error()},
		}
	}
	instID, err := s.cfg.GitHub.GetRepoInstallation(ctx, repoRef)
	if err != nil {
		return unavailable(err)
	}
	fc, err := s.cfg.GitHub.GetWorkflowSpec(ctx, forge.FromGitHubInstallationID(instID), repoRef, "")
	var registered []string
	switch {
	case errors.Is(err, forge.ErrNotFound):
		registered = []string{}
	case err != nil:
		return unavailable(err)
	default:
		parsed, perr := spec.ParseBytes(fc.Content)
		if perr != nil {
			return &campaignSourceRefusal{
				Status:  http.StatusUnprocessableEntity,
				Code:    "workflow_spec_invalid",
				Message: "the repository workflow spec on the default branch does not parse",
				Details: map[string]any{"repo": c.Repo, "requested": workflow, "error": perr.Error()},
			}
		}
		registered = make([]string, 0, len(parsed.Workflows))
		for wid := range parsed.Workflows {
			registered = append(registered, wid)
		}
		sort.Strings(registered)
		if _, ok := parsed.Workflows[workflow]; ok {
			return nil
		}
	}
	return &campaignSourceRefusal{
		Status: http.StatusUnprocessableEntity,
		Code:   "workflow_not_registered",
		Message: fmt.Sprintf("workflow %q is not declared by the repository spec on its default branch; name one of the registered workflows (any workflow other than %s needs a reason)",
			workflow, defaultHotfixWorkflow),
		Details: map[string]any{"requested": workflow, "registered": registered},
	}
}

// releaseCampaignHoldIfDue evaluates c's dispatch hold and releases it when
// due — its hotfix item is terminal, or the run linked to that item is, or the
// hold reached its expires_at on s.nowFunc's clock. It returns the hold that is
// STILL active afterwards (nil when none). A read error, or a release whose
// campaign_hold_released row cannot be recorded (wrapping
// campaign.ErrHoldReleaseUnrecorded), is returned with the still-active hold:
// callers fail closed. Idempotent under concurrency — a lost release race is
// the repository's no-op. trigger is one of the holdReleaseTrigger* values.
func (s *Server) releaseCampaignHoldIfDue(ctx context.Context, c *campaign.Campaign, trigger string) (*campaign.DispatchHold, error) {
	hold, err := s.cfg.CampaignRepo.GetDispatchHold(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("read dispatch hold: %w", err)
	}
	if hold == nil {
		return nil, nil
	}
	item, err := s.cfg.CampaignRepo.GetCampaignItem(ctx, hold.HotfixItemID)
	if err != nil {
		return hold, fmt.Errorf("read hotfix item: %w", err)
	}
	terminal := item.State.IsTerminal()
	if !terminal && item.RunID != nil && s.cfg.RunRepo != nil {
		rr, rerr := s.cfg.RunRepo.GetRun(ctx, *item.RunID)
		if rerr != nil {
			return hold, fmt.Errorf("read hotfix run: %w", rerr)
		}
		terminal = rr.State.IsTerminal()
	}
	now := s.nowFunc().UTC()
	reason, due := campaign.HoldReleaseReason(hold, terminal, now)
	if !due {
		return hold, nil
	}
	acct, err := s.cfg.CampaignRepo.GetCampaignAccountID(ctx, c.ID)
	if err != nil {
		return hold, fmt.Errorf("%w: resolve campaign account: %w", campaign.ErrHoldReleaseUnrecorded, err)
	}
	var acctID *uuid.UUID
	if acct != "" {
		u, perr := uuid.Parse(acct)
		if perr != nil {
			return hold, fmt.Errorf("%w: campaign account %q is not a UUID", campaign.ErrHoldReleaseUnrecorded, acct)
		}
		acctID = &u
	}
	_, err = s.cfg.CampaignRepo.ReleaseDispatchHold(ctx, campaign.ReleaseDispatchHoldParams{
		CampaignID:   c.ID,
		HotfixItemID: hold.HotfixItemID,
		Reason:       reason,
		Record: func(rctx context.Context, h campaign.DispatchHold) error {
			return s.appendSystemCampaignAuditStrict(rctx, acctID, categoryCampaignHoldReleased, map[string]any{
				"campaign_id":      c.ID.String(),
				"hotfix_item_id":   h.HotfixItemID.String(),
				"hotfix_issue_ref": item.IssueRef,
				"reason":           reason,
				"held_at":          h.HeldAt,
				"expires_at":       h.ExpiresAt,
				"released_at":      now,
				"trigger":          trigger,
				"actor":            campaignHoldSystemSubject,
			})
		},
	})
	if err != nil {
		return hold, err
	}
	// Released, or a concurrent release won: report whatever hold is active
	// now (a fresh hold placed after the release, or none).
	cur, err := s.cfg.CampaignRepo.GetDispatchHold(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("re-read dispatch hold: %w", err)
	}
	return cur, nil
}

// holdReleaseRefusal maps a releaseCampaignHoldIfDue error: an unrecordable
// release is 503 campaign_audit_unavailable (the hold stays active), anything
// else is 500 dependency_check_failed. hold, when known, puts the still-active
// hold's expires_at in the details AND the message: a 5xx body's details pass
// the default-deny redactor (errors.go) only for allow-listed keys, which
// expires_at is not, so the message is what carries it to the caller.
func holdReleaseRefusal(c *campaign.Campaign, hold *campaign.DispatchHold, err error) *campaignSourceRefusal {
	if errors.Is(err, campaign.ErrHoldReleaseUnrecorded) {
		details := map[string]any{"campaign_id": c.ID.String(), internalCauseKey: err.Error()}
		msg := "a due dispatch hold could not be released because its campaign_hold_released row could not be recorded; the hold stays active"
		if hold != nil {
			details["hotfix_item_id"] = hold.HotfixItemID.String()
			details["expires_at"] = hold.ExpiresAt
			msg += " (expires_at " + hold.ExpiresAt.UTC().Format(time.RFC3339) + ")"
		}
		return &campaignSourceRefusal{
			Status:  http.StatusServiceUnavailable,
			Code:    "campaign_audit_unavailable",
			Message: msg + " — cancel the hotfix run to unblock, and the hold releases once the audit chain recovers",
			Details: details,
		}
	}
	return &campaignSourceRefusal{
		Status:  http.StatusInternalServerError,
		Code:    "dependency_check_failed",
		Message: "reading the campaign dispatch hold failed",
		Details: map[string]any{"campaign_id": c.ID.String(), internalCauseKey: err.Error()},
	}
}

// appendSystemCampaignAuditStrict appends one SYSTEM-actor campaign-level
// audit entry on the global chain, stamped with the campaign's own account
// partition (not the caller's), and RETURNS its failure: it is a
// record-before-commit callback.
func (s *Server) appendSystemCampaignAuditStrict(ctx context.Context, acctID *uuid.UUID, category string, payload map[string]any) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("audit repository is not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", category, err)
	}
	kind := audit.ActorSystem
	subject := campaignHoldSystemSubject
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      body,
		AccountID:    acctID,
	}); err != nil {
		return fmt.Errorf("append %s: %w", category, err)
	}
	return nil
}
