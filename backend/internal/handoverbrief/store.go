package handoverbrief

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CampaignLister is the campaign read the brief needs, already projected to
// InFlightItem (Kind "campaign"). It is declared over this package's own
// types, NOT campaign.Repository's, because backend/internal/campaign's
// closure reaches backend/internal/workmgmt and mcpserver imports this
// package for the wire model and Bound — the ADR-064 guard
// (mcpserver TestNoBoardReadOnMCPToolSurface) forbids workmgmt on the MCP tool
// surface. The campaign.Repository adapter lives with the caller
// (server.campaignInFlightLister). accountID is "" for an unscoped read.
type CampaignLister interface {
	ListInFlightCampaigns(ctx context.Context, repo, accountID, state string, limit int) ([]InFlightItem, error)
}

// RunLister is the run read the brief needs (run.Repository satisfies it).
type RunLister interface {
	ListRuns(ctx context.Context, f run.ListRunsFilter) ([]*run.Run, error)
}

// Store reads the in_flight section over the existing repositories. Either
// collaborator may be nil: the brief then degrades that part alone
// (campaign_store_unconfigured / run_store_unconfigured).
type Store struct {
	campaigns CampaignLister
	runs      RunLister
}

// NewStore returns a Store over the given listers (either may be nil).
func NewStore(campaigns CampaignLister, runs RunLister) *Store {
	return &Store{campaigns: campaigns, runs: runs}
}

func accountArg(accountID *uuid.UUID) string {
	if accountID == nil {
		return ""
	}
	return accountID.String()
}

// Campaigns returns at most limit campaigns of repo in state, newest first
// (the repository's created_at DESC order), account-scoped. The caller passes
// limit+1 so the extra row IS the first omitted one.
func (s *Store) Campaigns(ctx context.Context, repo string, accountID *uuid.UUID, state string, limit int) ([]InFlightItem, error) {
	if s == nil || s.campaigns == nil {
		return nil, fmt.Errorf("handoverbrief: campaign repository unconfigured")
	}
	return s.campaigns.ListInFlightCampaigns(ctx, repo, accountArg(accountID), state, limit)
}

// Runs returns at most limit runs of repo in state, in the repository's list
// order, account-scoped. The caller passes limit+1.
func (s *Store) Runs(ctx context.Context, repo string, accountID *uuid.UUID, state string, limit int) ([]InFlightItem, error) {
	if s == nil || s.runs == nil {
		return nil, fmt.Errorf("handoverbrief: run repository unconfigured")
	}
	rows, err := s.runs.ListRuns(ctx, run.ListRunsFilter{
		Repo: repo, State: state, AccountID: accountArg(accountID), Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]InFlightItem, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		it := InFlightItem{Kind: "run", ID: r.ID, State: string(r.State), WorkflowID: r.WorkflowID, CreatedAt: r.CreatedAt.UTC()}
		if r.TriggerRef != nil {
			it.Ref = *r.TriggerRef
		}
		out = append(out, it)
	}
	return out, nil
}

func base64URL(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }
