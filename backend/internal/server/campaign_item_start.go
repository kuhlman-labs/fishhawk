package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// categoryCampaignItemRunAbandoned is the global-chain record that a campaign
// item start minted a run it could not link and cancelled it (#5266). It is
// registered in audit.KnownCategories as INTERNAL: actor system, global chain,
// not an issue-comment surface, and withheld-intent from run-bound readers
// (its payload names winner_run_id, another starter's run).
const categoryCampaignItemRunAbandoned = "campaign_item_run_abandoned"

// The campaign_item_run_abandoned reasons: why a minted run ended unlinked.
const (
	// itemRunAbandonClaimConflict: the claimed link found the item linked to
	// another run, or this start's claim replaced or released (claim_lost).
	itemRunAbandonClaimConflict = "claim_conflict"
	// itemRunAbandonLinkFailed: the claimed link failed for any other reason.
	itemRunAbandonLinkFailed = "link_failed"
	// itemRunAbandonTransitionFailed: the item's running transition failed
	// after the link, so the item was unlinked again.
	itemRunAbandonTransitionFailed = "transition_failed"
	// itemRunAbandonStartIncomplete: the run row committed but its stages
	// could not be created (*RunStartIncompleteError).
	itemRunAbandonStartIncomplete = "start_incomplete"
)

// The outcome key a response names an abandoned run under: cancelled_run_id
// ONLY when the cancel succeeded, cancel_failed_run_id otherwise. A response
// never carries both. Both are in redactableDetailKeys so they survive 5xx
// redaction.
const (
	itemStartOutcomeCancelled    = "cancelled_run_id"
	itemStartOutcomeCancelFailed = "cancel_failed_run_id"
)

// itemStartBudgets bounds an item start after its claim (#5266). Start bounds
// the mint, the claimed link, the running transition and the caller's
// post-start tail; Cleanup bounds the failure cleanup (the transition-failure
// unlink, the claim release, the run cancel and the abandon audit append),
// which runs on its OWN context. Production passes campaign.ItemStartBudget and
// campaign.ItemStartCleanupBudget; a zero field takes that default.
type itemStartBudgets struct {
	Start   time.Duration
	Cleanup time.Duration
}

func (b itemStartBudgets) withDefaults() itemStartBudgets {
	if b.Start <= 0 {
		b.Start = campaign.ItemStartBudget
	}
	if b.Cleanup <= 0 {
		b.Cleanup = campaign.ItemStartCleanupBudget
	}
	return b
}

// itemStartRequest is the input to startClaimedCampaignItemRun: the campaign
// and the (already eligibility-checked) item snapshot, the resolved run-start
// params, and the budgets.
type itemStartRequest struct {
	Campaign *campaign.Campaign
	Item     *campaign.Item
	Params   StartRunForCampaignIssueParams
	Budgets  itemStartBudgets
	// OnStarted, when set, runs after the item's running transition commits,
	// on the SAME detached, bounded start context as the mint (#5266 approval
	// condition X1), never on r.Context(). The handler passes its post-start
	// tail here (the campaign_issue_started emit and the pending /
	// awaiting_human derive), so a client disconnect after the start cannot
	// drop them.
	OnStarted func(ctx context.Context, runRow *run.Run, item *campaign.Item)
}

// startClaimedCampaignItemRun is the operator item start's claim-before-mint
// core (#5266). It claims the item (campaign_item_start_claims) BEFORE minting
// a run, so a losing starter never creates a run the host dispatcher could
// admit; mints the run; links it only while this start still holds its claim;
// and moves the item to running. On success it returns the minted, linked run
// and the running item, and the caller writes its 201 body. On failure it has
// already written the error response and returns ok=false.
//
//  1. Claim (request context). A linked conflict is 409 item_not_eligible
//     naming the winner (details.run_id); a live claim is 409
//     item_not_eligible with details.start_in_progress; a missing item is 404
//     campaign_item_not_found; any other error is 500. No refusal mints.
//     A claim whose remaining lifetime does not cover Start + Cleanup is
//     released and refused start_in_progress rather than minted (X4).
//  2. The start runs on context.WithTimeout(context.WithoutCancel(r.Context()),
//     Start), so a client disconnect never stops a start halfway.
//  3. Mint. A *RunStartIncompleteError abandons the run (start_incomplete) and
//     answers 500 campaign_run_start_incomplete; any other mint error releases
//     the claim and answers 502 campaign_run_start_failed.
//  4. Claimed link. A conflict abandons the run (claim_conflict) and answers
//     409 item_not_eligible; any other link error abandons it (link_failed)
//     and answers 500 internal_error.
//  5. Running transition. A failure unlinks the item, abandons the run
//     (transition_failed) and keeps the pre-#5266 status mapping.
//
// Every cleanup act runs on its own context (U3), created where cleanup
// begins and never derived from the start context, so a start that used up
// its budget still releases, cancels and records.
func (s *Server) startClaimedCampaignItemRun(w http.ResponseWriter, r *http.Request, req itemStartRequest) (*run.Run, *campaign.Item, bool) {
	c, item := req.Campaign, req.Item
	budgets := req.Budgets.withDefaults()
	base := func() map[string]any {
		return map[string]any{
			"campaign_id": c.ID.String(),
			"issue_ref":   item.IssueRef,
			"item_state":  string(item.State),
		}
	}

	// (1) Claim. The adapter reads the clock AFTER the item row lock is
	// granted (X4), so a lock wait never eats the claim's TTL.
	claim, err := s.cfg.CampaignRepo.ClaimCampaignItemStart(r.Context(), campaign.ClaimCampaignItemStartParams{
		ItemID:  item.ID,
		Starter: campaign.StarterOperator,
		Clock:   s.nowFunc,
	})
	if err != nil {
		var conflict *campaign.ItemStartConflictError
		switch {
		case errors.As(err, &conflict) && conflict.Reason == campaign.StartConflictLinked && conflict.WinnerRunID != nil:
			d := base()
			d["run_id"] = conflict.WinnerRunID.String()
			s.writeError(w, r, http.StatusConflict, "item_not_eligible",
				"the campaign item is already linked to run "+conflict.WinnerRunID.String()+"; drive that run instead of starting another", d)
		case errors.As(err, &conflict):
			d := base()
			d["start_in_progress"] = true
			if conflict.HeldClaim != nil {
				d["claim_starter"] = conflict.HeldClaim.Starter
				d["claim_expires_at"] = conflict.HeldClaim.ExpiresAt.UTC().Format(time.RFC3339Nano)
			}
			s.writeError(w, r, http.StatusConflict, "item_not_eligible",
				"another start of this item is in progress; poll fishhawk_get_campaign_status for its run_id instead of retrying", d)
		case errors.Is(err, campaign.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "campaign_item_not_found",
				"campaign item not found", map[string]any{"campaign_id": c.ID.String(), "issue_ref": item.IssueRef})
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"claim campaign item start failed", map[string]any{"error": err.Error(), "item_id": item.ID.String()})
		}
		return nil, nil, false
	}

	// X4: refuse to mint under a claim that could expire mid-start. The claim
	// is this start's own, so release it before refusing.
	if !claim.Covers(s.nowFunc(), budgets.Start+budgets.Cleanup) {
		s.releaseItemStartClaimDetached(r.Context(), budgets, c, item, claim)
		d := base()
		d["start_in_progress"] = true
		d["claim_starter"] = claim.Starter
		d["claim_expires_at"] = claim.ExpiresAt.UTC().Format(time.RFC3339Nano)
		d["claim_budget_short"] = true
		s.writeError(w, r, http.StatusConflict, "item_not_eligible",
			"this start's item claim no longer covers the start and cleanup budgets, so no run was minted; poll fishhawk_get_campaign_status before starting the item again", d)
		return nil, nil, false
	}

	// (2) The start runs detached from the request, under its own budget.
	startCtx, cancelStart := context.WithTimeout(context.WithoutCancel(r.Context()), budgets.Start)
	defer cancelStart()

	// (3) Mint.
	runRow, err := s.StartRunForCampaignIssue(startCtx, req.Params)
	if err != nil {
		var incomplete *RunStartIncompleteError
		if errors.As(err, &incomplete) {
			key := s.cleanupAbandonedItemStart(r.Context(), budgets, c, item, claim, incomplete.RunID, itemRunAbandonStartIncomplete, nil, false)
			s.writeError(w, r, http.StatusInternalServerError, "campaign_run_start_incomplete",
				"the backend minted a run for the campaign item but could not create its stages; the run was not linked to the item",
				map[string]any{"run_id": incomplete.RunID.String(), "issue_ref": item.IssueRef, key: incomplete.RunID.String(), "error": err.Error()})
			return nil, nil, false
		}
		s.releaseItemStartClaimDetached(r.Context(), budgets, c, item, claim)
		s.writeError(w, r, http.StatusBadGateway, "campaign_run_start_failed",
			"could not start a run for the campaign item",
			map[string]any{"error": err.Error(), "issue_ref": item.IssueRef})
		return nil, nil, false
	}

	// (4) Link only while this start still holds its claim.
	if _, err := s.cfg.CampaignRepo.LinkClaimedCampaignItemRun(startCtx, campaign.LinkClaimedCampaignItemRunParams{
		ItemID: item.ID, ClaimID: claim.ClaimID, RunID: runRow.ID,
	}); err != nil {
		var conflict *campaign.ItemStartConflictError
		if errors.As(err, &conflict) {
			key := s.cleanupAbandonedItemStart(r.Context(), budgets, c, item, claim, runRow.ID, itemRunAbandonClaimConflict, conflict.WinnerRunID, false)
			d := map[string]any{"campaign_id": c.ID.String(), "issue_ref": item.IssueRef, key: runRow.ID.String()}
			if conflict.WinnerRunID != nil {
				d["run_id"] = conflict.WinnerRunID.String()
			}
			if conflict.Reason == campaign.StartConflictClaimLost {
				d["claim_lost"] = true
			}
			s.writeError(w, r, http.StatusConflict, "item_not_eligible",
				"another starter won this campaign item while this start was minting; the run this call minted was abandoned", d)
			return nil, nil, false
		}
		key := s.cleanupAbandonedItemStart(r.Context(), budgets, c, item, claim, runRow.ID, itemRunAbandonLinkFailed, nil, false)
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"link campaign item to run failed",
			map[string]any{"error": err.Error(), "item_id": item.ID.String(), "run_id": runRow.ID.String(), key: runRow.ID.String()})
		return nil, nil, false
	}

	// (5) Running transition. On failure the item is unlinked so it
	// re-partitions as Eligible, and the minted run is abandoned.
	updatedItem, err := s.cfg.CampaignRepo.TransitionCampaignItem(startCtx, item.ID, campaign.ItemStateRunning)
	if err != nil {
		key := s.cleanupAbandonedItemStart(r.Context(), budgets, c, item, claim, runRow.ID, itemRunAbandonTransitionFailed, nil, true)
		var inv campaign.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_transition",
				err.Error(), map[string]any{"item_id": item.ID.String(), key: runRow.ID.String()})
			return nil, nil, false
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"transition campaign item to running failed",
			map[string]any{"error": err.Error(), "item_id": item.ID.String(), key: runRow.ID.String()})
		return nil, nil, false
	}

	if req.OnStarted != nil {
		req.OnStarted(startCtx, runRow, updatedItem)
	}
	return runRow, updatedItem, true
}

// newItemStartCleanupContext returns the context an item start's cleanup runs
// on (#5266 U3): detached from parent's cancellation and deadline
// (context.WithoutCancel keeps its values, so the caller's identity still
// scopes the audit append) and bounded by the cleanup budget alone. Callers
// pass the REQUEST context, never the start context: a start that used up its
// budget must still release, cancel and record.
func newItemStartCleanupContext(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), budget)
}

// releaseItemStartClaimDetached releases this start's claim on its own
// cleanup context, derived from parent (the REQUEST context, never the start
// context). Release is idempotent and claim-scoped, so it is safe on
// every failure path; a release failure WARN-logs (the claim then expires
// after ItemStartClaimTTL).
func (s *Server) releaseItemStartClaimDetached(parent context.Context, budgets itemStartBudgets, c *campaign.Campaign, item *campaign.Item, claim *campaign.StartClaim) {
	ctx, cancel := newItemStartCleanupContext(parent, budgets.Cleanup)
	defer cancel()
	s.releaseItemStartClaim(ctx, c, item, claim)
}

func (s *Server) releaseItemStartClaim(ctx context.Context, c *campaign.Campaign, item *campaign.Item, claim *campaign.StartClaim) {
	if err := s.cfg.CampaignRepo.ReleaseCampaignItemStart(ctx, item.ID, claim.ClaimID); err != nil {
		s.cfg.Logger.Warn("campaign item start claim release failed; the claim blocks the item until it expires",
			"campaign_id", c.ID.String(), "item_id", item.ID.String(), "claim_id", claim.ClaimID.String(),
			"expires_at", claim.ExpiresAt.UTC().Format(time.RFC3339Nano), "error", err.Error())
	}
}

// cleanupAbandonedItemStart runs a failed start's cleanup on its own context
// (U3), derived from parent (the REQUEST context, never the start context):
// the transition-failure unlink when unlink is set, the abandon (cancel,
// then audit) and the claim release. It returns the outcome key the response
// names the abandoned run under.
func (s *Server) cleanupAbandonedItemStart(parent context.Context, budgets itemStartBudgets, c *campaign.Campaign, item *campaign.Item, claim *campaign.StartClaim, runID uuid.UUID, reason string, winner *uuid.UUID, unlink bool) string {
	ctx, cancel := newItemStartCleanupContext(parent, budgets.Cleanup)
	defer cancel()
	if unlink {
		if _, err := s.cfg.CampaignRepo.SetCampaignItemRun(ctx, item.ID, nil); err != nil {
			s.cfg.Logger.Warn("campaign item-run unlink after failed running transition also failed; item left linked-but-not-running",
				"campaign_id", c.ID.String(), "item_id", item.ID.String(), "run_id", runID.String(), "error", err.Error())
		}
	}
	cancelled := s.abandonCampaignItemRun(ctx, c, item, runID, reason, winner)
	s.releaseItemStartClaim(ctx, c, item, claim)
	if cancelled {
		return itemStartOutcomeCancelled
	}
	return itemStartOutcomeCancelFailed
}

// abandonCampaignItemRun cancels a minted campaign item run that ended
// unlinked, then appends campaign_item_run_abandoned with the TRUE outcome
// (#5266). The cancel acts first and never depends on the audit; an append
// failure WARN-logs every payload field and the caller still reports the
// truthful outcome. It returns whether the cancel succeeded.
//
// What the cancel does NOT do (U4c): it only moves the run to cancelled. It
// does not unwind a stage the host dispatcher admitted in the mint-to-abandon
// window (its run_auto_driven row, its host-dispatch marker or a spawned
// runner), and it does not undo the run_started board move. Claim-before-mint
// means a LOSING starter never mints, so no loser run is ever marked; but the
// WINNER's run carries the auto_dispatch mark from its CreateRun commit (the
// driver always stamps it; the operator path only with auto_dispatch:true), so
// a marked run IS unlinked from that commit until LinkClaimedCampaignItemRun
// commits, and again from a transition-failure unlink until this cancel
// commits. The host dispatcher's campaign gate permits an unlinked run and its
// eligibility admits a marked one, so a tick in that window (normally
// milliseconds) can admit the run's pending plan stage; later ticks refuse the
// run as terminal. A residual this helper cannot reach: a CreateRun whose
// commit raced the start deadline and then returned an error leaves a
// committed, unlinked, possibly marked run whose id no caller learns, so
// nobody cancels it (#5266 X6c).
func (s *Server) abandonCampaignItemRun(ctx context.Context, c *campaign.Campaign, item *campaign.Item, runID uuid.UUID, reason string, winner *uuid.UUID) bool {
	cancelled := true
	if err := s.CancelCampaignRun(ctx, runID); err != nil {
		cancelled = false
		s.cfg.Logger.Warn("cancelling an abandoned campaign item run failed; the run is left live for operator cancellation",
			"campaign_id", c.ID.String(), "item_id", item.ID.String(), "run_id", runID.String(), "reason", reason, "error", err.Error())
	}
	payload := map[string]any{
		"campaign_id": c.ID.String(),
		"issue_ref":   item.IssueRef,
		"item_id":     item.ID.String(),
		"run_id":      runID.String(),
		"starter":     campaign.StarterOperator,
		"reason":      reason,
		"cancelled":   cancelled,
	}
	winnerStr := ""
	if winner != nil {
		winnerStr = winner.String()
		payload["winner_run_id"] = winnerStr
	}
	if err := s.appendCampaignItemRunAbandoned(ctx, payload); err != nil {
		s.cfg.Logger.Warn("append campaign_item_run_abandoned failed; the abandon is recorded only in this log line",
			"campaign_id", c.ID.String(), "issue_ref", item.IssueRef, "item_id", item.ID.String(),
			"run_id", runID.String(), "starter", campaign.StarterOperator, "reason", reason,
			"winner_run_id", winnerStr, "cancelled", cancelled, "error", err.Error())
	}
	return cancelled
}

// appendCampaignItemRunAbandoned appends the abandon row to the global chain
// under the system actor, returning the append error (unlike the best-effort
// emitCampaignAudit) so the caller can log the payload on failure.
func (s *Server) appendCampaignItemRunAbandoned(ctx context.Context, payload map[string]any) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("no audit repository configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	systemKind := audit.ActorSystem
	_, err = s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  categoryCampaignItemRunAbandoned,
		ActorKind: &systemKind,
		Payload:   body,
		AccountID: identityAccountID(ctx),
	})
	return err
}

// CancelCampaignRun cancels a campaign item run that ended unlinked (#5266):
// a state transition to cancelled, never a delete (the
// abandonUnauditedOverrideRun precedent). The campaign driver reaches it
// through its RunCanceller seam.
func (s *Server) CancelCampaignRun(ctx context.Context, runID uuid.UUID) error {
	if s.cfg.RunRepo == nil {
		return errors.New("cancel campaign run: no run repository configured")
	}
	_, err := s.cfg.RunRepo.TransitionRun(ctx, runID, run.StateCancelled)
	return err
}
