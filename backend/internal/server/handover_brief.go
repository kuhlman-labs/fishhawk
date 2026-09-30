package server

// The handover brief surface (E76.4 / #3767, ADR-083 #3751, ADR-082 #3728
// rule 7). GET /v0/handover-brief composes backend/internal/handoverbrief over
// the SAME collaborators the digest, captain, campaign and run surfaces
// already use, with the GET /v0/digest posture posture-for-posture:
// read:audit, a named 501 when a required collaborator is unwired, repo and
// section 400s, the repo-visibility 403, then Compose + Bound at
// handoverbrief.DefaultByteBudget. It NEVER writes and mints no audit entry.
//
// The same composition runs at offer time (composeOfferBrief, called by
// handleCaptainVerb BEFORE captain.Store.Apply): the canonical brief's
// brief_hash and window are stamped onto the captain_handover_offered
// payload. A section read error is a DEGRADATION — the brief still hashes. Only
// a failure to ESTABLISH the brief (captain record / window read, or a missing
// required collaborator) records brief_unavailable with a reason and an empty
// hash, and the offer still proceeds: refusing a handover over a read-only
// projection would strand the seat.
//
// Long-form contract: backend/internal/server/README.md § "Handover brief".

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/handoverbrief"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// scopeHandoverBriefRead gates GET /v0/handover-brief: the brief is a
// projection of the audit chain, so it takes the digest's EXISTING read
// scope (read:audit) — no new scope, so no token loses access.
const scopeHandoverBriefRead = scopeDigestRead

// handoverBriefConfigured writes the 501 handover_brief_unconfigured envelope
// naming every missing REQUIRED collaborator and returns false. The campaign
// and run repositories are optional: their absence degrades in_flight (and
// the delegation read) inside the brief instead.
func (s *Server) handoverBriefConfigured(w http.ResponseWriter, r *http.Request) bool {
	var missing []string
	if s.cfg.DigestStore == nil {
		missing = append(missing, "digest_store")
	}
	if s.cfg.DigestIndex == nil {
		missing = append(missing, "decision_index")
	}
	if s.cfg.CaptainStore == nil {
		missing = append(missing, "captain_store")
	}
	if len(missing) == 0 {
		return true
	}
	// The missing collaborators ride the MESSAGE: writeError redacts every
	// non-allow-listed 5xx detail key.
	s.writeError(w, r, http.StatusNotImplemented, "handover_brief_unconfigured",
		"the handover brief surface is not wired on this deployment; missing: "+strings.Join(missing, ", "), nil)
	return false
}

// handoverBriefDeps assembles Compose's collaborators. Every nil check is
// explicit so a nil interface never becomes a typed-nil lister, and a nil
// *captain.Store never becomes a non-nil SnapshotReader.
func (s *Server) handoverBriefDeps(ctx context.Context, repo string) handoverbrief.Deps {
	deps := handoverbrief.Deps{Digest: digest.Deps{Store: s.cfg.DigestStore, Index: s.cfg.DigestIndex}}
	if s.cfg.CaptainStore != nil {
		deps.Captain = s.cfg.CaptainStore
	}
	var campaigns handoverbrief.CampaignLister
	if s.cfg.CampaignRepo != nil {
		campaigns = campaignInFlightLister{repo: s.cfg.CampaignRepo}
	}
	var runs handoverbrief.RunLister
	if s.cfg.RunRepo != nil {
		runs = s.cfg.RunRepo
	}
	deps.InFlight = handoverbrief.NewStore(campaigns, runs)
	deps.Delegation, deps.DelegationUnavailableReason = s.handoverBriefDelegation(ctx, repo)
	return deps
}

// campaignInFlightLister adapts campaign.Repository to
// handoverbrief.CampaignLister. The adapter lives HERE, not in handoverbrief,
// because backend/internal/campaign's closure reaches workmgmt and mcpserver
// imports handoverbrief — the ADR-064 guard forbids workmgmt on the MCP tool
// surface (mcpserver TestNoBoardReadOnMCPToolSurface).
type campaignInFlightLister struct{ repo campaign.Repository }

// ListInFlightCampaigns returns at most limit campaigns of repo in state,
// newest first, account-scoped (accountID "" = unscoped), projected to the
// brief's in-flight item.
func (l campaignInFlightLister) ListInFlightCampaigns(ctx context.Context, repo, accountID, state string, limit int) ([]handoverbrief.InFlightItem, error) {
	rows, err := l.repo.ListCampaigns(ctx, campaign.ListCampaignsFilter{
		Repo: repo, State: state, AccountID: accountID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]handoverbrief.InFlightItem, 0, len(rows))
	for _, c := range rows {
		if c == nil {
			continue
		}
		out = append(out, handoverbrief.InFlightItem{Kind: "campaign", ID: c.ID, State: string(c.State), Ref: c.EpicRef, CreatedAt: c.CreatedAt.UTC()})
	}
	return out, nil
}

// handoverBriefDelegation projects the delegation view from the spec cached
// on the repository's NEWEST run — the source=run_cache read of GET
// /v0/repos/{owner}/{name}/delegation, same account narrowing. It never
// fetches through the forge: the brief composes on every offer, and a forge
// round-trip there would couple a handover to forge availability. A nil view
// comes back with a named reason, which the brief records as its
// delegation_unavailable degradation.
func (s *Server) handoverBriefDelegation(ctx context.Context, repo string) (*delegationview.View, string) {
	if s.cfg.RunRepo == nil {
		return nil, "no run repository is configured, so no cached workflow spec can be read"
	}
	rows, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{Repo: repo, AccountID: IdentityFrom(ctx).AccountID, Limit: 1})
	if err != nil {
		return nil, "list runs failed: " + err.Error()
	}
	if len(rows) == 0 || len(bytes.TrimSpace(rows[0].WorkflowSpec)) == 0 {
		return nil, "no run of this repository carries a cached workflow spec"
	}
	specBytes := rows[0].WorkflowSpec
	parsed, err := spec.ParseBytes(specBytes)
	if err != nil {
		return nil, "the cached workflow spec does not validate: " + err.Error()
	}
	v := &delegationview.View{Repo: repo, Source: delegationSourceRunCache, WorkflowSHA: rows[0].WorkflowSHA}
	var raw map[string]any
	if yaml.Unmarshal(specBytes, &raw) == nil {
		if ver, ok := raw["version"]; ok && ver != nil {
			v.SpecVersion = fmt.Sprint(ver)
		}
	}
	v.SchemaMajor = spec.VersionMajor(v.SpecVersion)
	v.Workflows = delegationview.Project(parsed)
	v.ContentHash = delegationview.HashWorkflows(v.Workflows)
	return v, ""
}

// composeOfferBrief composes the brief an offer records, OUTSIDE the
// captain lock (Apply's decide callback runs inside the advisory-locked
// transaction, so no read may move there). The chain can advance between
// this compose and the append; the recorded hash and window describe what
// was composed — and shown — at offer time.
func (s *Server) composeOfferBrief(ctx context.Context, repo, captainSubject, successor string) captain.OfferBrief {
	b, err := handoverbrief.Compose(ctx, s.handoverBriefDeps(ctx, repo), handoverbrief.Request{
		Repo: repo, AccountID: identityAccountID(ctx), Subject: captainSubject, Successor: successor,
	})
	if err != nil {
		reason := handoverbrief.UnavailableReason(err)
		if reason == "" {
			reason = "compose_failed"
		}
		return captain.OfferBrief{Unavailable: true, UnavailableReason: reason}
	}
	return captain.OfferBrief{Hash: b.BriefHash, FromSequence: b.Window.FromSequence, ToSequence: b.Window.ToSequence}
}

// writeHandoverBriefError maps a handoverbrief error onto the wire: a
// malformed request (including a to_sequence above the chain head) is 400
// validation_failed; an unestablishable brief is 503
// handover_brief_unavailable naming its reason; anything else is 500.
func (s *Server) writeHandoverBriefError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, handoverbrief.ErrInvalidRequest):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(), nil)
	case errors.Is(err, handoverbrief.ErrUnavailable):
		// The reason rides the MESSAGE (5xx detail keys are redacted).
		s.writeError(w, r, http.StatusServiceUnavailable, "handover_brief_unavailable",
			"the handover brief could not be established ("+handoverbrief.UnavailableReason(err)+")", nil)
	default:
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"handover brief failed", map[string]any{"error": err.Error()})
	}
}

// handleGetHandoverBrief serves
// GET /v0/handover-brief?repo=&section=&from_sequence=&to_sequence=.
func (s *Server) handleGetHandoverBrief(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeHandoverBriefRead) {
		return
	}
	if !s.handoverBriefConfigured(w, r) {
		return
	}
	q := r.URL.Query()
	repo := q.Get("repo")
	if repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required (owner/name)", map[string]any{"field": "repo"})
		return
	}
	section, err := handoverbrief.ParseSection(q.Get("section"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(),
			map[string]any{"field": "section"})
		return
	}
	from, ok := s.parseDigestSequence(w, r, "from_sequence")
	if !ok {
		return
	}
	to, ok := s.parseDigestSequence(w, r, "to_sequence")
	if !ok {
		return
	}
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}
	ctx := r.Context()
	b, err := handoverbrief.Compose(ctx, s.handoverBriefDeps(ctx, repo), handoverbrief.Request{
		Repo: repo, AccountID: identityAccountID(ctx), Subject: IdentityFrom(ctx).Subject,
		FromSequence: from, ToSequence: to,
	})
	if err != nil {
		s.writeHandoverBriefError(w, r, err)
		return
	}
	if b, err = handoverbrief.Select(b, section); err != nil {
		s.writeHandoverBriefError(w, r, err)
		return
	}
	bounded, err := handoverbrief.Bound(b, handoverbrief.DefaultByteBudget)
	if err != nil {
		s.writeHandoverBriefError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, bounded)
}
