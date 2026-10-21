package server

// campaign_dispatch_hold.go is the host-dispatch half of the campaign hotfix
// verb (E83.100 / #4234): the campaign DISPATCH HOLD the marker enforces, and
// the auto-promote of an expedited implement waiter to the head of its local
// concurrency group.
//
// Placement (ADR-094 / ADR-100): the hold is a campaign-level PRE-admission
// refusal at the host-dispatch marker. It runs after the marker's state switch
// and beside the interim host-memory hold, BEFORE the CAS / slot admission, so
// a refusal commits no stage transition and no queue row, and ADR-094's
// resource-group admission is unchanged. The auto-promote is a priority boost
// through #4191's audited promote core, not a gate: it fails OPEN.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// maxCampaignAncestorDepth bounds the ancestor walk that resolves a stage's
// campaign item: at most this many DecomposedFrom / ParentRunID hops.
const maxCampaignAncestorDepth = 8

// Why the ancestor walk could not reach a root. Each is treated as HELD.
const (
	campaignAncestryMissingParent = "missing_parent"
	campaignAncestryCycle         = "cycle"
	campaignAncestryDepthExceeded = "depth_exceeded"
)

// campaignHotfixPromoteSubject is the actor subject of an auto-promote's
// stage_concurrency_promoted row (actor_kind system).
const campaignHotfixPromoteSubject = "system:campaign-hotfix-promote"

// campaignAncestry is a run's campaign membership, resolved by
// resolveCampaignAncestry.
type campaignAncestry struct {
	// items are the campaign items linked to the run, or to its nearest
	// campaign-linked ancestor. Empty: not a campaign run.
	items []*campaign.Item
	// unresolved names why the walk failed (a campaignAncestry* value); "" when
	// it reached either a campaign-linked run or a root.
	unresolved string
}

// runParentID is the run's ancestry link: DecomposedFrom (a fan-out child's
// parent), else ParentRunID (a follow-up run's predecessor).
func runParentID(r *run.Run) *uuid.UUID {
	if r.DecomposedFrom != nil {
		return r.DecomposedFrom
	}
	return r.ParentRunID
}

// resolveCampaignAncestry walks from runRow up its DecomposedFrom /
// ParentRunID links to the first run that is linked to a campaign item, so a
// decomposed child of a campaign item's run resolves to that item. A run with
// no link and no item is not a campaign run. A missing parent, a cycle or
// more than maxCampaignAncestorDepth hops sets unresolved, which the guard
// treats as HELD (fail closed). A read error other than a missing parent is
// returned.
func (s *Server) resolveCampaignAncestry(ctx context.Context, runRow *run.Run) (campaignAncestry, error) {
	cur := runRow
	seen := map[uuid.UUID]bool{cur.ID: true}
	for hops := 0; ; hops++ {
		items, err := s.cfg.CampaignRepo.ListCampaignItemsForRun(ctx, cur.ID)
		if err != nil {
			return campaignAncestry{}, fmt.Errorf("list campaign items for run %s: %w", cur.ID, err)
		}
		if len(items) > 0 {
			return campaignAncestry{items: items}, nil
		}
		parent := runParentID(cur)
		if parent == nil {
			return campaignAncestry{}, nil
		}
		if seen[*parent] {
			return campaignAncestry{unresolved: campaignAncestryCycle}, nil
		}
		if hops >= maxCampaignAncestorDepth {
			return campaignAncestry{unresolved: campaignAncestryDepthExceeded}, nil
		}
		next, err := s.cfg.RunRepo.GetRun(ctx, *parent)
		if errors.Is(err, run.ErrNotFound) {
			return campaignAncestry{unresolved: campaignAncestryMissingParent}, nil
		}
		if err != nil {
			return campaignAncestry{}, fmt.Errorf("read ancestor run %s: %w", *parent, err)
		}
		seen[next.ID] = true
		cur = next
	}
}

// guardCampaignDispatchHold is the marker's campaign dispatch hold. It is
// inert unless stage is an IMPLEMENT stage and a campaign repository is wired.
// It resolves the stage's campaign items through the ancestor walk. EVERY
// item carrying an expedite record (the hotfix that placed the hold, and any
// other expedited item) is exempt, and so are its decomposed children. For
// every other item it releases a due hold first (releaseCampaignHoldIfDue,
// trigger host_dispatch), then refuses 409 campaign_dispatch_held while a
// hold is still active. An unresolvable ancestry is HELD too.
//
// It returns the expedited item the stage resolves to (nil when none), which
// admitGroupedStage uses for the auto-promote, or a refusal. Read errors
// answer 500 dependency_check_failed and an unrecordable release answers 503
// campaign_audit_unavailable: both fail closed.
func (s *Server) guardCampaignDispatchHold(ctx context.Context, runRow *run.Run, stage *run.Stage) (*campaign.Item, *campaignSourceRefusal) {
	if s.cfg.CampaignRepo == nil || stage.Type != run.StageTypeImplement {
		return nil, nil
	}
	readFailed := func(err error) *campaignSourceRefusal {
		return &campaignSourceRefusal{
			Status:  http.StatusInternalServerError,
			Code:    "dependency_check_failed",
			Message: "could not resolve the stage's campaign membership or dispatch hold",
			Details: map[string]any{"run_id": runRow.ID.String(), "stage_id": stage.ID.String(), internalCauseKey: err.Error()},
		}
	}
	anc, err := s.resolveCampaignAncestry(ctx, runRow)
	if err != nil {
		return nil, readFailed(err)
	}
	if anc.unresolved != "" {
		return nil, &campaignSourceRefusal{
			Status: http.StatusConflict,
			Code:   "campaign_dispatch_held",
			Message: fmt.Sprintf("the stage's campaign membership could not be resolved (%s), so its implement admission is held: a campaign dispatch hold is enforced fail-closed",
				anc.unresolved),
			Details: map[string]any{"run_id": runRow.ID.String(), "stage_id": stage.ID.String(),
				"reason": "campaign_ancestry_unresolved", "unresolved": anc.unresolved},
		}
	}
	var expedited *campaign.Item
	checked := map[uuid.UUID]bool{}
	for _, it := range anc.items {
		if it.Expedite != nil {
			if expedited == nil {
				expedited = it
			}
			continue
		}
		if checked[it.CampaignID] {
			continue
		}
		checked[it.CampaignID] = true
		c, err := s.cfg.CampaignRepo.GetCampaign(ctx, it.CampaignID)
		if err != nil {
			return nil, readFailed(fmt.Errorf("read campaign %s: %w", it.CampaignID, err))
		}
		hold, err := s.releaseCampaignHoldIfDue(ctx, c, holdReleaseTriggerHostDispatch)
		if err != nil {
			return nil, holdReleaseRefusal(c, hold, err)
		}
		if hold == nil {
			continue
		}
		hot, err := s.cfg.CampaignRepo.GetCampaignItem(ctx, hold.HotfixItemID)
		if err != nil {
			return nil, readFailed(fmt.Errorf("read hotfix item %s: %w", hold.HotfixItemID, err))
		}
		return nil, &campaignSourceRefusal{
			Status: http.StatusConflict,
			Code:   "campaign_dispatch_held",
			Message: fmt.Sprintf("campaign %s holds new implement dispatches until its hotfix %s is terminal or the hold expires at %s; running stages continue, and this stage is dispatchable once the hold releases",
				c.ID, hot.IssueRef, hold.ExpiresAt.UTC().Format(time.RFC3339)),
			Details: map[string]any{
				"campaign_id":      c.ID.String(),
				"hotfix_item_id":   hold.HotfixItemID.String(),
				"hotfix_issue_ref": hot.IssueRef,
				"expires_at":       hold.ExpiresAt,
				"stage_id":         stage.ID.String(),
				"item_id":          it.ID.String(),
			},
		}
	}
	return expedited, nil
}

// autoPromoteExpeditedWaiter moves a QUEUED implement stage of an expedited
// campaign item to the head of its concurrency group through the audited
// promote core, and returns adm with the post-promote position and holders.
// It acts only when the stage is behind another waiter (position > 1) and is
// not already promoted in this queue episode, so an operator's later promote
// of another waiter is not fought. The reason is a fixed template naming the
// item's issue ref and campaign id; the operator's free-text hotfix reason is
// never copied into it. Fail OPEN: a status read or promote error logs WARN
// and returns adm unchanged (the stage keeps its FIFO standing and the next
// marker re-POST retries).
func (s *Server) autoPromoteExpeditedWaiter(ctx context.Context, stage *run.Stage, item *campaign.Item, adm concurrency.Admission) concurrency.Admission {
	if adm.Position <= 1 {
		return adm
	}
	warn := func(msg string, err error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, msg,
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("campaign_id", item.CampaignID.String()),
			slog.String("error", err.Error()))
	}
	statuses, err := s.cfg.Concurrency.StatusForStages(ctx, []uuid.UUID{stage.ID})
	if err != nil {
		warn("host-dispatch: expedited waiter status unreadable; keeping its FIFO position", err)
		return adm
	}
	if st, ok := statuses[stage.ID]; !ok || st.State != concurrency.SlotQueued || st.Promoted {
		return adm
	}
	reason := fmt.Sprintf("campaign hotfix: expedited item %s of campaign %s (#4234)", item.IssueRef, item.CampaignID)
	p, _, err := s.promoteQueuedStageRecorded(ctx, stage.ID, reason, audit.ActorSystem, campaignHotfixPromoteSubject)
	if err != nil {
		warn("host-dispatch: expedited waiter auto-promote failed; keeping its FIFO position", err)
		return adm
	}
	if p.Changed {
		adm.Position = p.NewPosition
		adm.Holders = p.Holders
	}
	return adm
}
