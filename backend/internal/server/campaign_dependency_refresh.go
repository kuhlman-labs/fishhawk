package server

// campaign_dependency_refresh.go is the MONOTONE campaign dependency refresh
// (E86.47 / #4358): a campaign's DAG is no longer frozen at assembly. Native
// blocked-by links and `Depends-on:` lines are re-read for members that have
// not started, and any edge the tracker now reports that the persisted
// depends_on lacks is ADDED — never removed — through the same repository
// transaction the append verb uses (campaign.AppendCampaignItems), recorded
// before commit as campaign_dependencies_refreshed.
//
// Two triggers. GET /status runs it for every unstarted member, single-flight
// per campaign with a 30 s minimum interval, and never fails the read. The
// start verb runs it for the TARGET item only and fails CLOSED: starting after
// an unobserved or unrecorded dependency change is the hazard. Both run only
// for a caller whose account owns the campaign (or the campaign is untenanted)
// and who can see the repo, so a read can never let a foreign caller drive a
// forge read or a write.
//
// Monotone means a tracker edit can only HOLD an item, never release one:
// removing a native link or a Depends-on line changes nothing persisted. An
// edge onto a non-member is held and marked external; an unparsable target is
// reported and never written; a running item is skipped and reported, never
// interrupted; a cycle closed by tracker edges among members is held (both
// items stay blocked) and named, never released.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
)

// categoryCampaignDependenciesRefreshed is the audit category written, before
// commit, by every refresh that adds an edge. Registered in
// audit/categories.go.
const categoryCampaignDependenciesRefreshed = "campaign_dependencies_refreshed"

// campaignDependencyRefreshMinInterval is the status trigger's minimum spacing
// per campaign: a refresh that finished (successfully OR on a forge error)
// less than this long ago is not re-run by a status poll. The start trigger is
// not gated.
const campaignDependencyRefreshMinInterval = 30 * time.Second

// Refresh triggers, carried on the audit row.
const (
	refreshTriggerStatus = "status"
	refreshTriggerStart  = "start"
)

// Skip reasons reported on the status response's dependency_refresh block.
const (
	refreshSkippedInFlight      = "in_flight"
	refreshSkippedMinInterval   = "min_interval"
	refreshSkippedForeign       = "foreign_account"
	refreshSkippedRepoInvisible = "repo_not_visible"
	refreshSkippedNoCandidates  = "no_candidates"
)

// errDependencyRefreshForge: the tracker read failed, so nothing was observed.
// errDependencyRefreshUnrecorded: an observed change could not be recorded on
// the audit chain, so the transaction rolled back and nothing was written.
// errDependencyRefreshPersist: any other repository failure, nothing written.
var (
	errDependencyRefreshForge      = errors.New("campaign dependency refresh: tracker read failed")
	errDependencyRefreshUnrecorded = errors.New("campaign dependency refresh: change could not be recorded")
	errDependencyRefreshPersist    = errors.New("campaign dependency refresh: change could not be written")
)

// dependencyRefreshOpts selects the trigger. target narrows the candidates to
// one item (the start trigger). strictRecord makes an unrecordable change an
// error the caller must fail closed on; without it the caller still gets the
// observed additions so it can HOLD them in its response.
type dependencyRefreshOpts struct {
	trigger      string
	target       *uuid.UUID
	strictRecord bool
}

// dependencyRefreshAddition is one item's newly observed edges. External are
// the subset onto non-members (held, marked external).
type dependencyRefreshAddition struct {
	IssueRef string   `json:"issue_ref"`
	Refs     []string `json:"refs"`
	External []string `json:"external,omitempty"`
}

// dependencyRefreshResult is what one refresh observed and did.
type dependencyRefreshResult struct {
	Performed     bool
	SkippedReason string
	// Added are the observed additions. When Committed is false they were NOT
	// persisted (the caller holds them in its response only).
	Added          []dependencyRefreshAddition
	Committed      bool
	SkippedStarted []string
	Unparsable     []string
	CycleMembers   []string
	Warnings       []string
	// observed is Added keyed by item id, for the caller's hold overlay.
	observed map[uuid.UUID][]string
}

// dependencyRefreshPayload is the status response's dependency_refresh block.
type dependencyRefreshPayload struct {
	Performed      bool                        `json:"performed"`
	SkippedReason  string                      `json:"skipped_reason,omitempty"`
	Added          []dependencyRefreshAddition `json:"added,omitempty"`
	Committed      bool                        `json:"committed"`
	SkippedStarted []string                    `json:"skipped_started,omitempty"`
	Unparsable     []string                    `json:"unparsable,omitempty"`
	CycleMembers   []string                    `json:"cycle_members,omitempty"`
	Warnings       []string                    `json:"warnings,omitempty"`
}

func (r *dependencyRefreshResult) payload() dependencyRefreshPayload {
	return dependencyRefreshPayload{
		Performed:      r.Performed,
		SkippedReason:  r.SkippedReason,
		Added:          r.Added,
		Committed:      r.Committed,
		SkippedStarted: r.SkippedStarted,
		Unparsable:     r.Unparsable,
		CycleMembers:   r.CycleMembers,
		Warnings:       r.Warnings,
	}
}

// campaignDependencyRefreshGate is the status trigger's per-campaign
// single-flight + minimum-interval gate. Process-local by design.
type campaignDependencyRefreshGate struct {
	mu    sync.Mutex
	slots map[uuid.UUID]*campaignDependencyRefreshSlot
}

type campaignDependencyRefreshSlot struct {
	inFlight     bool
	lastFinished time.Time
}

// tryAcquire claims the campaign's slot without blocking. It refuses with
// in_flight while another refresh runs, and with min_interval when the last
// one finished less than campaignDependencyRefreshMinInterval before now.
func (g *campaignDependencyRefreshGate) tryAcquire(id uuid.UUID, now time.Time) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.slots == nil {
		g.slots = map[uuid.UUID]*campaignDependencyRefreshSlot{}
	}
	sl := g.slots[id]
	if sl == nil {
		sl = &campaignDependencyRefreshSlot{}
		g.slots[id] = sl
	}
	if sl.inFlight {
		return false, refreshSkippedInFlight
	}
	if !sl.lastFinished.IsZero() && now.Sub(sl.lastFinished) < campaignDependencyRefreshMinInterval {
		return false, refreshSkippedMinInterval
	}
	sl.inFlight = true
	return true, ""
}

// release stamps lastFinished on EVERY completion — success, no-op, forge
// error or audit failure alike — so a failing tracker is retried at most once
// per interval rather than on every poll.
func (g *campaignDependencyRefreshGate) release(id uuid.UUID, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sl := g.slots[id]; sl != nil {
		sl.inFlight = false
		sl.lastFinished = now
	}
}

// campaignRefreshAccess reports whether the caller may drive a refresh of c:
// its account owns c (or c is untenanted — the GetCampaignAccountID predicate
// enforceCampaignAccount applies) AND the repo-visibility filter admits c.Repo.
// Any lookup error denies. It writes nothing: the refresh is skipped, the
// caller's own request proceeds unchanged.
func (s *Server) campaignRefreshAccess(ctx context.Context, c *campaign.Campaign) (bool, string) {
	acct, err := s.cfg.CampaignRepo.GetCampaignAccountID(ctx, c.ID)
	if err != nil || (acct != "" && IdentityFrom(ctx).AccountID != acct) {
		return false, refreshSkippedForeign
	}
	f, err := s.repoFilterFor(ctx)
	if err != nil {
		return false, refreshSkippedRepoInvisible
	}
	if ok, err := f.allows(ctx, c.Repo); err != nil || !ok {
		return false, refreshSkippedRepoInvisible
	}
	return true, ""
}

// refreshCampaignDependenciesForStatus is the GET /status trigger: access
// gate, then the single-flight + minimum-interval gate, then a lenient
// refresh. It never fails the read. Returns nil when the deployment disables
// the refresh.
func (s *Server) refreshCampaignDependenciesForStatus(ctx context.Context, c *campaign.Campaign, items []*campaign.Item) *dependencyRefreshResult {
	if !s.cfg.CampaignDependencyRefresh {
		return nil
	}
	if ok, reason := s.campaignRefreshAccess(ctx, c); !ok {
		return &dependencyRefreshResult{SkippedReason: reason}
	}
	ok, reason := s.campaignRefreshGate.tryAcquire(c.ID, s.nowFunc())
	if !ok {
		return &dependencyRefreshResult{SkippedReason: reason}
	}
	defer func() { s.campaignRefreshGate.release(c.ID, s.nowFunc()) }()
	res, err := s.refreshCampaignDependencies(ctx, c, items, dependencyRefreshOpts{trigger: refreshTriggerStatus})
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "campaign dependency refresh failed on status read; serving the persisted DAG",
			"campaign_id", c.ID.String(), "error", err.Error())
	}
	return res
}

// refreshStartTarget is the start trigger: a STRICT refresh of the one target
// item. It returns ok=false after writing the refusal (502 on a tracker read
// failure, 503 when the change could not be recorded or written — nothing was
// started and nothing written). On success it returns the re-listed items when
// an edge was added, else nil. A foreign or repo-invisible caller, a disabled
// deployment, or a started target skip it with zero forge reads and writes.
func (s *Server) refreshStartTarget(w http.ResponseWriter, r *http.Request, c *campaign.Campaign, item *campaign.Item, items []*campaign.Item) ([]*campaign.Item, bool) {
	if !s.cfg.CampaignDependencyRefresh || item.RunID != nil ||
		(item.State != campaign.ItemStatePending && item.State != campaign.ItemStateBlocked) {
		return nil, true
	}
	if ok, _ := s.campaignRefreshAccess(r.Context(), c); !ok {
		return nil, true
	}
	target := item.ID
	res, err := s.refreshCampaignDependencies(r.Context(), c, items, dependencyRefreshOpts{
		trigger: refreshTriggerStart, target: &target, strictRecord: true,
	})
	switch {
	case errors.Is(err, errDependencyRefreshForge):
		s.writeError(w, r, http.StatusBadGateway, "campaign_dependency_refresh_failed",
			"the item's dependencies could not be re-read from the tracker, so nothing was started; retry shortly",
			map[string]any{"campaign_id": c.ID.String(), "issue_ref": item.IssueRef, internalCauseKey: err.Error()})
		return nil, false
	case err != nil:
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_dependency_audit_failed",
			"a newly observed dependency could not be recorded, so nothing was written and nothing was started; retry shortly",
			map[string]any{"campaign_id": c.ID.String(), "issue_ref": item.IssueRef, internalCauseKey: err.Error()})
		return nil, false
	}
	if !res.Committed || len(res.Added) == 0 {
		return nil, true
	}
	relisted, lerr := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), c.ID)
	if lerr != nil {
		// The edge IS committed; without a fresh list the gate below would read
		// the pre-refresh DAG, so hold the observed edges on the stale slice.
		return holdObservedEdges(items, &dependencyRefreshResult{observed: res.observed}), true
	}
	return relisted, true
}

// refreshCampaignDependencies re-reads the tracker for the unstarted members
// (only opts.target on the start trigger) and adds every newly observed edge.
// It is a no-op unless Config.CampaignDependencyRefresh. When nothing is new
// it makes NO repository call and writes NO audit row.
//
// On error the result is still non-nil: a forge error observed nothing; an
// unrecordable or unwritable change carries the observed additions with
// Committed=false so a lenient caller can hold them in its response.
func (s *Server) refreshCampaignDependencies(ctx context.Context, c *campaign.Campaign, items []*campaign.Item, opts dependencyRefreshOpts) (*dependencyRefreshResult, error) {
	res := &dependencyRefreshResult{}
	if !s.cfg.CampaignDependencyRefresh {
		return res, nil
	}
	members := make(map[string]*campaign.Item, len(items))
	for _, it := range items {
		members[it.IssueRef] = it
	}
	var candidates []string
	for _, it := range items {
		if opts.target != nil && it.ID != *opts.target {
			continue
		}
		if (it.State == campaign.ItemStatePending || it.State == campaign.ItemStateBlocked) &&
			it.RunID == nil && campaign.ValidDependencyRef(it.IssueRef) {
			candidates = append(candidates, it.IssueRef)
		}
	}
	if len(candidates) == 0 {
		res.SkippedReason = refreshSkippedNoCandidates
		return res, nil
	}
	res.Performed = true

	src := createCampaignRequest{Repo: c.Repo, Items: candidates}
	epicRef, owner, name, refusal := validateCampaignSourceShape(src)
	if refusal == nil {
		var resolution *campaignSourceResolution
		resolution, refusal = s.resolveCampaignSource(ctx, time.Now(), src, epicRef, owner, name)
		if refusal == nil {
			return s.applyObservedDependencies(ctx, c, members, resolution, opts, res)
		}
	}
	res.Warnings = append(res.Warnings, "tracker read failed ("+refusal.Code+"); the persisted DAG is served unchanged")
	return res, fmt.Errorf("%w: %s: %s", errDependencyRefreshForge, refusal.Code, refusal.Message)
}

// applyObservedDependencies maps a resolution onto per-item additions and
// writes them through the append transaction.
func (s *Server) applyObservedDependencies(ctx context.Context, c *campaign.Campaign, members map[string]*campaign.Item, resolution *campaignSourceResolution, opts dependencyRefreshOpts, res *dependencyRefreshResult) (*dependencyRefreshResult, error) {
	observed := map[int][]string{}
	unparsable := map[string]bool{}
	note := func(from int, to string) {
		if !campaign.ValidDependencyRef(to) {
			unparsable[to] = true
			return
		}
		observed[from] = append(observed[from], to)
	}
	r := resolution.Result
	for _, e := range r.Edges {
		note(e.From, issueRefString(e.To))
	}
	for _, e := range r.DroppedEdges {
		note(e.From, e.TargetRef())
	}
	for _, e := range r.SatisfiedEdges {
		// A closed-as-completed MEMBER still gates by its member state; a
		// satisfied non-member holds nothing.
		if to := issueRefString(e.To); members[to] != nil {
			note(e.From, to)
		}
	}

	additions := map[uuid.UUID][]string{}
	external := map[string][]string{}
	for from, refs := range observed {
		it := members[issueRefString(from)]
		if it == nil {
			continue
		}
		var add []string
		for _, ref := range campaign.MergeDependsOn(nil, refs) {
			if containsRef(it.DependsOn, ref) || ref == it.IssueRef {
				continue
			}
			add = append(add, ref)
			if members[ref] == nil {
				external[it.IssueRef] = append(external[it.IssueRef], ref)
			}
		}
		if len(add) > 0 {
			additions[it.ID] = add
		}
	}
	for ref := range unparsable {
		res.Unparsable = append(res.Unparsable, ref)
	}
	sort.Strings(res.Unparsable)
	if len(additions) == 0 {
		// Nothing new: no repository call, no audit row.
		res.Committed = true
		return res, nil
	}
	res.observed = additions
	res.CycleMembers = heldCycleMembers(members, additions)
	res.Added = describeAdditions(members, additions, external)

	out, err := s.cfg.CampaignRepo.AppendCampaignItems(ctx, campaign.AppendCampaignItemsParams{
		CampaignID:      c.ID,
		AddDependencies: additions,
		// Never admitted here: AllowExternal only governs NEW items, and a
		// refresh appends none. External edges on existing members are HELD.
		AllowExternal: false,
		Record: func(rctx context.Context, out campaign.AppendOutcome) error {
			if len(out.Added) == 0 {
				return nil // every target started under the lock: nothing written
			}
			added := make([]map[string]any, 0, len(out.Added))
			for _, a := range out.Added {
				ext := []string{}
				for _, ref := range a.Refs {
					if containsRef(external[a.IssueRef], ref) {
						ext = append(ext, ref)
					}
				}
				added = append(added, map[string]any{"issue_ref": a.IssueRef, "refs": a.Refs, "external": ext})
			}
			return s.appendCampaignAuditStrict(rctx, categoryCampaignDependenciesRefreshed, map[string]any{
				"campaign_id":     c.ID.String(),
				"repo":            c.Repo,
				"trigger":         opts.trigger,
				"added":           added,
				"skipped_started": nonNilStrings(out.SkippedStarted),
				"unparsable":      nonNilStrings(res.Unparsable),
				"cycle_members":   nonNilStrings(res.CycleMembers),
			})
		},
	})
	if err != nil {
		if errors.Is(err, campaign.ErrAppendUnrecorded) {
			res.Warnings = append(res.Warnings, "the "+categoryCampaignDependenciesRefreshed+" audit row could not be written, so the observed blockers were NOT persisted; they are held in this response only and the next refresh retries")
			return res, fmt.Errorf("%w: %w", errDependencyRefreshUnrecorded, err)
		}
		res.Warnings = append(res.Warnings, "the observed blockers could not be written; they are held in this response only and the next refresh retries")
		return res, fmt.Errorf("%w: %w", errDependencyRefreshPersist, err)
	}
	res.Committed = true
	res.SkippedStarted = out.SkippedStarted
	if len(out.SkippedStarted) > 0 {
		// Re-describe from what was actually written.
		applied := make(map[uuid.UUID][]string, len(out.Added))
		for _, a := range out.Added {
			applied[a.ItemID] = a.Refs
		}
		res.observed = applied
		res.Added = describeAdditions(members, applied, external)
	}
	return res, nil
}

// holdObservedEdges returns items with every uncommitted observed addition
// merged into its depends_on (a copy; the inputs are not mutated), so a
// lenient caller whose write failed still reports a newly blocked item as
// HELD rather than eligible. Started items are left as persisted.
func holdObservedEdges(items []*campaign.Item, res *dependencyRefreshResult) []*campaign.Item {
	if res == nil || res.Committed || len(res.observed) == 0 {
		return items
	}
	out := make([]*campaign.Item, 0, len(items))
	for _, it := range items {
		add := res.observed[it.ID]
		if len(add) == 0 || it.RunID != nil ||
			(it.State != campaign.ItemStatePending && it.State != campaign.ItemStateBlocked) {
			out = append(out, it)
			continue
		}
		cp := *it
		cp.DependsOn = campaign.MergeDependsOn(it.DependsOn, add)
		out = append(out, &cp)
	}
	return out
}

// heldCycleMembers names the members that sit on a depends_on cycle once the
// additions are merged: a cycle closed by tracker edges is HELD (its items
// stay blocked), and reported so an operator can break it.
func heldCycleMembers(members map[string]*campaign.Item, additions map[uuid.UUID][]string) []string {
	graph := make(map[string][]string, len(members))
	for ref, it := range members {
		graph[ref] = campaign.MergeDependsOn(it.DependsOn, additions[it.ID])
	}
	var onCycle []string
	for ref := range graph {
		seen := map[string]bool{}
		stack := append([]string{}, graph[ref]...)
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n == ref {
				onCycle = append(onCycle, ref)
				break
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, graph[n]...)
		}
	}
	sort.Strings(onCycle)
	return onCycle
}

func describeAdditions(members map[string]*campaign.Item, additions map[uuid.UUID][]string, external map[string][]string) []dependencyRefreshAddition {
	var out []dependencyRefreshAddition
	for ref, it := range members {
		add := additions[it.ID]
		if len(add) == 0 {
			continue
		}
		var ext []string
		for _, r := range add {
			if containsRef(external[ref], r) {
				ext = append(ext, r)
			}
		}
		out = append(out, dependencyRefreshAddition{IssueRef: ref, Refs: add, External: ext})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IssueRef < out[j].IssueRef })
	return out
}

func issueRefString(n int) string { return fmt.Sprintf("issue:%d", n) }
