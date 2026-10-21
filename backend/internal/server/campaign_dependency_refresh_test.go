package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// refreshRepo wraps the fake campaign repo to observe and perturb the refresh's
// AppendCampaignItems call: it records every params value, can start an item
// "under the lock" (beforeAppend), and can fail the call outright.
type refreshRepo struct {
	*fakeCampaignRepo
	mu           sync.Mutex
	appendCalls  []campaign.AppendCampaignItemsParams
	beforeAppend func()
	appendErr    error
	// snapshot makes ListCampaignItemsForCampaign return value COPIES, as the
	// Postgres adapter does, so a caller holding a stale list does not see a
	// later write through shared pointers.
	snapshot bool
	// accountErr fails GetCampaignAccountID (a store fault on the access probe).
	accountErr error
}

func (r *refreshRepo) GetCampaignAccountID(ctx context.Context, id uuid.UUID) (string, error) {
	if r.accountErr != nil {
		return "", r.accountErr
	}
	return r.fakeCampaignRepo.GetCampaignAccountID(ctx, id)
}

func (r *refreshRepo) ListCampaignItemsForCampaign(ctx context.Context, id uuid.UUID) ([]*campaign.Item, error) {
	items, err := r.fakeCampaignRepo.ListCampaignItemsForCampaign(ctx, id)
	if err != nil || !r.snapshot {
		return items, err
	}
	out := make([]*campaign.Item, 0, len(items))
	for _, it := range items {
		cp := *it
		cp.DependsOn = append([]string(nil), it.DependsOn...)
		out = append(out, &cp)
	}
	return out, nil
}

func (r *refreshRepo) AppendCampaignItems(ctx context.Context, p campaign.AppendCampaignItemsParams) (*campaign.AppendOutcome, error) {
	r.mu.Lock()
	r.appendCalls = append(r.appendCalls, p)
	before, aerr := r.beforeAppend, r.appendErr
	r.mu.Unlock()
	if before != nil {
		before()
	}
	if aerr != nil {
		return nil, aerr
	}
	return r.fakeCampaignRepo.AppendCampaignItems(ctx, p)
}

func (r *refreshRepo) appendCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.appendCalls)
}

// categoryFailingAudit records every global append but FAILS the one category
// named, so only the refresh's record-before-commit decides an outcome.
type categoryFailingAudit struct {
	campaignAuditRecorder
	fail string
}

func (a *categoryFailingAudit) AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	if p.Category == a.fail {
		return nil, errors.New("audit chain unavailable")
	}
	return a.campaignAuditRecorder.AppendGlobalChained(ctx, p)
}

// testClock is a settable clock wired into Server.nowFunc.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type refreshHarness struct {
	*appendHarness
	rr  *refreshRepo
	clk *testClock
}

func newRefreshHarness(t *testing.T, cfg Config) *refreshHarness {
	t.Helper()
	cfg.CampaignDependencyRefresh = true
	h := &refreshHarness{appendHarness: newAppendHarness(t, cfg), clk: &testClock{t: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}}
	h.rr = &refreshRepo{fakeCampaignRepo: h.repo}
	h.s.cfg.CampaignRepo = h.rr
	h.s.nowFunc = h.clk.now
	return h
}

type refreshStatusBody struct {
	campaignStatusBody
	DependencyRefresh *dependencyRefreshPayload `json:"dependency_refresh"`
}

func getRefreshStatus(t *testing.T, s *Server, id uuid.UUID, ident Identity) (int, refreshStatusBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+id.String()+"/status", nil)
	req.SetPathValue("campaign_id", id.String())
	w := httptest.NewRecorder()
	s.handleGetCampaignStatus(w, withIdentity(req, ident))
	var b refreshStatusBody
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
			t.Fatalf("decode status: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, b
}

func mustStatus(t *testing.T, s *Server, id uuid.UUID) refreshStatusBody {
	t.Helper()
	code, b := getRefreshStatus(t, s, id, testOperatorIdentity())
	if code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", code)
	}
	return b
}

func (h *appendHarness) itemDeps(id uuid.UUID, ref string) []string {
	h.repo.mu.Lock()
	defer h.repo.mu.Unlock()
	for _, it := range h.repo.itemsByCmp[id] {
		if it.IssueRef == ref {
			return append([]string{}, it.DependsOn...)
		}
	}
	return nil
}

func (h *appendHarness) auditPayloads(category string) []map[string]any {
	h.aud.mu.Lock()
	defer h.aud.mu.Unlock()
	var out []map[string]any
	for _, e := range h.aud.entries {
		if e.Category == category {
			var m map[string]any
			_ = json.Unmarshal(e.Payload, &m)
			out = append(out, m)
		}
	}
	return out
}

// TestStatus_PostStartBlockedByHoldsItem: a blocked-by added on the tracker
// after start is observed by a status poll and holds the item until its
// blocker succeeds. CF1-b vehicle (delete the status refresh call).
func TestStatus_PostStartBlockedByHoldsItem(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))

	st := mustStatus(t, h.s, c.ID)
	if !containsRef(st.Rollup.Eligible, "issue:10") {
		t.Fatalf("before the tracker edge, issue:10 should be eligible: %+v", st.Rollup)
	}
	// Nothing new observed: no repository call, no audit row (condition 4).
	if h.rr.appendCount() != 0 || h.aud.count(categoryCampaignDependenciesRefreshed) != 0 {
		t.Fatalf("no-op refresh: append calls %d, audit rows %d, want 0/0", h.rr.appendCount(), h.aud.count(categoryCampaignDependenciesRefreshed))
	}

	h.res.deps[10] = []int{11}
	h.clk.advance(campaignDependencyRefreshMinInterval)
	st = mustStatus(t, h.s, c.ID)
	if containsRef(st.Rollup.Eligible, "issue:10") {
		t.Fatalf("issue:10 still eligible after the tracker added blocked-by issue:11: %+v", st.Rollup)
	}
	if got := h.itemDeps(c.ID, "issue:10"); !containsRef(got, "issue:11") {
		t.Errorf("committed depends_on = %v, want issue:11 added", got)
	}
	if st.DependencyRefresh == nil || !st.DependencyRefresh.Committed || len(st.DependencyRefresh.Added) != 1 {
		t.Errorf("dependency_refresh = %+v", st.DependencyRefresh)
	}
	rows := h.auditPayloads(categoryCampaignDependenciesRefreshed)
	if len(rows) != 1 {
		t.Fatalf("campaign_dependencies_refreshed rows = %d, want 1", len(rows))
	}
	for _, k := range []string{"campaign_id", "repo", "trigger", "added", "skipped_started", "unparsable", "cycle_members"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("audit payload missing %q: %v", k, rows[0])
		}
	}
	if rows[0]["trigger"] != refreshTriggerStatus {
		t.Errorf("trigger = %v", rows[0]["trigger"])
	}

	// The blocker succeeds → the held item becomes eligible.
	h.repo.mu.Lock()
	for _, it := range h.repo.itemsByCmp[c.ID] {
		if it.IssueRef == "issue:11" {
			it.State = campaign.ItemStateSucceeded
		}
	}
	h.repo.mu.Unlock()
	st = mustStatus(t, h.s, c.ID)
	if !containsRef(st.Rollup.Eligible, "issue:10") {
		t.Errorf("after issue:11 succeeded, issue:10 should be eligible: %+v", st.Rollup)
	}
}

// TestRefresh_RemovedTrackerEdgeKeepsItemHeld: removing a Depends-on line on
// the tracker never releases a persisted edge. The second arm swaps the line
// for two edges onto SUCCEEDED members so a write happens: replace semantics
// would leave only satisfied edges and release the item (CF1-a vehicle).
func TestRefresh_RemovedTrackerEdgeKeepsItemHeld(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(
		cItem("issue:10", []string{"issue:11"}, campaign.ItemStatePending),
		cItem("issue:11", nil, campaign.ItemStatePending),
		cItem("issue:12", nil, campaign.ItemStateSucceeded),
		cItem("issue:13", nil, campaign.ItemStateSucceeded),
	)
	st := mustStatus(t, h.s, c.ID)
	if containsRef(st.Rollup.Eligible, "issue:10") {
		t.Fatalf("tracker reports no edge: issue:10 must stay held on issue:11: %+v", st.Rollup)
	}
	if got := h.itemDeps(c.ID, "issue:10"); len(got) != 1 || got[0] != "issue:11" {
		t.Fatalf("depends_on = %v, want [issue:11] unchanged", got)
	}

	h.res.deps[10] = []int{12, 13}
	h.clk.advance(campaignDependencyRefreshMinInterval)
	st = mustStatus(t, h.s, c.ID)
	if containsRef(st.Rollup.Eligible, "issue:10") {
		t.Fatalf("issue:10 released after the tracker dropped its issue:11 line: %+v", st.Rollup)
	}
	got := h.itemDeps(c.ID, "issue:10")
	for _, want := range []string{"issue:11", "issue:12", "issue:13"} {
		if !containsRef(got, want) {
			t.Errorf("depends_on = %v, want %s retained/added (monotone)", got, want)
		}
	}
}

// TestRefresh_ExternalHeldMarkedExternal: a tracker edge onto a non-member is
// held, marked external, and never admitted through AllowExternal.
func TestRefresh_ExternalHeldMarkedExternal(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	h.res.deps[10] = []int{999}
	st := mustStatus(t, h.s, c.ID)
	if containsRef(st.Rollup.Eligible, "issue:10") {
		t.Fatalf("issue:10 eligible despite an open external blocker: %+v", st.Rollup)
	}
	if got := h.itemDeps(c.ID, "issue:10"); !containsRef(got, "issue:999") {
		t.Errorf("depends_on = %v, want issue:999 held", got)
	}
	for _, p := range h.rr.appendCalls {
		if p.AllowExternal || len(p.NewItems) != 0 {
			t.Errorf("refresh passed AllowExternal=%v NewItems=%d, want false/0", p.AllowExternal, len(p.NewItems))
		}
	}
	rows := h.auditPayloads(categoryCampaignDependenciesRefreshed)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d", len(rows))
	}
	added, _ := rows[0]["added"].([]any)
	first, _ := added[0].(map[string]any)
	ext, _ := first["external"].([]any)
	if len(ext) != 1 || ext[0] != "issue:999" {
		t.Errorf("audit external = %v, want [issue:999]", first["external"])
	}
	if a := st.DependencyRefresh.Added; len(a) != 1 || len(a[0].External) != 1 {
		t.Errorf("response added = %+v, want issue:999 marked external", a)
	}
}

// TestRefresh_RunningItemNeverInterrupted: a running item is never a refresh
// candidate, and one that starts between the list and the lock is skipped and
// reported with its depends_on byte-unchanged.
func TestRefresh_RunningItemNeverInterrupted(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	runID := uuid.New()
	c := h.seed(
		cItemWithRun("issue:10", nil, campaign.ItemStateRunning, runID),
		cItem("issue:11", nil, campaign.ItemStatePending),
		cItem("issue:12", nil, campaign.ItemStatePending),
	)
	h.res.deps[10] = []int{11}
	h.res.deps[12] = []int{11}
	// issue:12 starts under the lock, after the refresh listed it.
	h.rr.beforeAppend = func() {
		h.repo.mu.Lock()
		defer h.repo.mu.Unlock()
		for _, it := range h.repo.itemsByCmp[c.ID] {
			if it.IssueRef == "issue:12" {
				it.State = campaign.ItemStateRunning
				rid := uuid.New()
				it.RunID = &rid
			}
		}
	}
	st := mustStatus(t, h.s, c.ID)
	for _, asked := range h.res.asked {
		if containsRef(asked, "issue:10") {
			t.Errorf("running issue:10 was resolved as a candidate: %v", asked)
		}
	}
	if got := h.itemDeps(c.ID, "issue:10"); len(got) != 0 {
		t.Errorf("running item depends_on = %v, want unchanged (empty)", got)
	}
	if got := h.itemDeps(c.ID, "issue:12"); len(got) != 0 {
		t.Errorf("started-under-lock item depends_on = %v, want unchanged (empty)", got)
	}
	if st.DependencyRefresh == nil || !containsRef(st.DependencyRefresh.SkippedStarted, "issue:12") {
		t.Errorf("dependency_refresh = %+v, want issue:12 in skipped_started", st.DependencyRefresh)
	}
	if n := h.aud.count(categoryCampaignDependenciesRefreshed); n != 0 {
		t.Errorf("audit rows = %d, want 0 (nothing was written)", n)
	}
}

// scriptedResolver answers one fixed resolution.
type scriptedResolver struct {
	name  string
	res   *workmgmt.EpicChildrenResult
	calls int
}

func (p *scriptedResolver) Name() string { return p.name }
func (p *scriptedResolver) File(context.Context, workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	return &workmgmt.CreatedItem{Provider: p.name}, nil
}
func (p *scriptedResolver) ResolveDependencies(context.Context, workmgmt.IssueSetRequest) (*workmgmt.EpicChildrenResult, error) {
	p.calls++
	return p.res, nil
}

// TestRefresh_UnparsableTargetNeverWritten: an unresolvable tracker token is
// reported and never written; the item is not held on it (CF1-f vehicle — the
// repository's own ValidDependencyRef masks committed state, so the vehicle is
// the response: no warning, item eligible, token reported).
func TestRefresh_UnparsableTargetNeverWritten(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	sr := &scriptedResolver{name: h.res.name, res: &workmgmt.EpicChildrenResult{
		Children:     []workmgmt.EpicChild{{Number: 10}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 10, ToRef: "not a ref", ToRefDigest: "0123456789abcdef", Reason: workmgmt.DropNotChild}},
	}}
	workmgmt.Register(sr)
	st := mustStatus(t, h.s, c.ID)
	if sr.calls != 1 {
		t.Fatalf("resolver calls = %d", sr.calls)
	}
	if got := h.itemDeps(c.ID, "issue:10"); len(got) != 0 {
		t.Errorf("depends_on = %v, want nothing written", got)
	}
	if h.rr.appendCount() != 0 {
		t.Errorf("append calls = %d, want 0", h.rr.appendCount())
	}
	if st.DependencyRefresh == nil || len(st.DependencyRefresh.Unparsable) != 1 || len(st.DependencyRefresh.Warnings) != 0 {
		t.Errorf("dependency_refresh = %+v, want the token reported, no warning", st.DependencyRefresh)
	}
	if !containsRef(st.Rollup.Eligible, "issue:10") {
		t.Errorf("rollup = %+v, want issue:10 eligible (an unparsable token holds nothing)", st.Rollup)
	}
}

// TestRefresh_TrackerCycleAmongMembersHeld: a cycle closed by tracker edges
// among existing members is held (both blocked) and named, never refused.
func TestRefresh_TrackerCycleAmongMembersHeld(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
	h.res.deps[10] = []int{11}
	h.res.deps[11] = []int{10}
	st := mustStatus(t, h.s, c.ID)
	if len(st.Rollup.Eligible) != 0 {
		t.Errorf("rollup eligible = %v, want both held", st.Rollup.Eligible)
	}
	if cm := st.DependencyRefresh.CycleMembers; len(cm) != 2 {
		t.Errorf("cycle_members = %v, want both", cm)
	}
	if !containsRef(h.itemDeps(c.ID, "issue:10"), "issue:11") || !containsRef(h.itemDeps(c.ID, "issue:11"), "issue:10") {
		t.Errorf("both edges must be committed")
	}
}

// TestStatus_RefreshSingleFlightMinInterval: one resolver round per 30 s per
// campaign; an in-flight refresh skips; a forge error stamps lastFinished too,
// so a failing tracker is retried at most once per interval (CF1-e vehicle).
func TestStatus_RefreshSingleFlightMinInterval(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	mustStatus(t, h.s, c.ID)
	st := mustStatus(t, h.s, c.ID)
	if n := h.res.callCount(); n != 1 {
		t.Fatalf("resolver calls after two polls inside 30 s = %d, want 1", n)
	}
	if st.DependencyRefresh.SkippedReason != refreshSkippedMinInterval {
		t.Errorf("skipped_reason = %q, want min_interval", st.DependencyRefresh.SkippedReason)
	}
	h.clk.advance(campaignDependencyRefreshMinInterval)
	mustStatus(t, h.s, c.ID)
	if n := h.res.callCount(); n != 2 {
		t.Fatalf("resolver calls after 30 s = %d, want 2", n)
	}

	// In flight: another refresh holds the slot.
	h.clk.advance(campaignDependencyRefreshMinInterval)
	if ok, _, _ := h.s.campaignRefreshGate.tryAcquire(c.ID, h.clk.now()); !ok {
		t.Fatal("could not claim the slot")
	}
	st = mustStatus(t, h.s, c.ID)
	if st.DependencyRefresh.SkippedReason != refreshSkippedInFlight || h.res.callCount() != 2 {
		t.Errorf("in flight: reason %q calls %d, want in_flight/2", st.DependencyRefresh.SkippedReason, h.res.callCount())
	}
	h.s.campaignRefreshGate.release(c.ID, h.clk.now(), nil, nil)

	// Forge error: lastFinished is stamped, so the next poll inside 30 s skips.
	h.clk.advance(campaignDependencyRefreshMinInterval)
	h.res.err = errors.New("tracker down")
	st = mustStatus(t, h.s, c.ID)
	if h.res.callCount() != 3 || len(st.DependencyRefresh.Warnings) == 0 {
		t.Fatalf("forge error: calls %d warnings %v", h.res.callCount(), st.DependencyRefresh.Warnings)
	}
	st = mustStatus(t, h.s, c.ID)
	if h.res.callCount() != 3 || st.DependencyRefresh.SkippedReason != refreshSkippedMinInterval {
		t.Errorf("after a forge error: calls %d reason %q, want 3/min_interval", h.res.callCount(), st.DependencyRefresh.SkippedReason)
	}
	h.clk.advance(campaignDependencyRefreshMinInterval)
	mustStatus(t, h.s, c.ID)
	if h.res.callCount() != 4 {
		t.Errorf("retry after the interval: calls %d, want 4", h.res.callCount())
	}
}

// TestStatus_ForeignAccountSkipsRefresh: a caller from another account reads
// the status (unchanged) but drives no forge read and no write.
func TestStatus_ForeignAccountSkipsRefresh(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
	h.repo.accounts[c.ID] = uuid.NewString()
	h.res.deps[10] = []int{11}
	st := mustStatus(t, h.s, c.ID)
	if h.res.callCount() != 0 || h.rr.appendCount() != 0 {
		t.Errorf("foreign caller drove calls: resolver %d append %d, want 0/0", h.res.callCount(), h.rr.appendCount())
	}
	if st.DependencyRefresh == nil || st.DependencyRefresh.SkippedReason != refreshSkippedForeign {
		t.Errorf("dependency_refresh = %+v, want foreign_account", st.DependencyRefresh)
	}
}

// TestStatus_RepoInvisibleSkipsRefresh: a same-account caller that cannot see
// the repo drives no forge read and no write. The read itself is already
// denied (403) by the status handler's own visibility guard, so the refresh's
// predicate is pinned directly too (it is the only guard on the start path).
func TestStatus_RepoInvisibleSkipsRefresh(t *testing.T) {
	h := newRefreshHarness(t, Config{
		AccountRoles:   fakeAccountRoles{role: account.RoleMember},
		RepoVisibility: newFakeRepoVisibility(map[string]bool{}),
	})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
	h.res.deps[10] = []int{11}
	if code, _ := getRefreshStatus(t, h.s, c.ID, memberIdentity()); code != http.StatusForbidden {
		t.Errorf("status code = %d, want 403", code)
	}
	ctx := context.WithValue(context.Background(), ctxKeyIdentity, memberIdentity())
	items, _ := h.repo.ListCampaignItemsForCampaign(ctx, c.ID)
	res := h.s.refreshCampaignDependenciesForStatus(ctx, c, items)
	if res == nil || res.SkippedReason != refreshSkippedRepoInvisible {
		t.Errorf("refresh = %+v, want repo_not_visible", res)
	}
	if h.res.callCount() != 0 || h.rr.appendCount() != 0 {
		t.Errorf("repo-invisible caller drove calls: resolver %d append %d", h.res.callCount(), h.rr.appendCount())
	}
}

// TestStatus_AuditFailureKeepsHoldWarns: an unrecordable change rolls back
// (committed depends_on unchanged) but the response still HOLDS the newly
// blocked item and names the missing audit row (condition 3). A write failure
// of any other kind holds the same way. The hold outlives the failing poll:
// polls the interval gate skips, and a poll whose tracker read fails, still
// hold it and warn, until a refresh records it.
func TestStatus_AuditFailureKeepsHoldWarns(t *testing.T) {
	for name, setup := range map[string]func(*refreshHarness){
		"audit": func(*refreshHarness) {},
		"write": func(h *refreshHarness) { h.rr.appendErr = errors.New("db down") },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{}
			if name == "audit" {
				cfg.AuditRepo = failingAuditRepo{}
			}
			h := newRefreshHarness(t, cfg)
			setup(h)
			c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
			h.res.deps[10] = []int{11}
			st := mustStatus(t, h.s, c.ID)
			if got := h.itemDeps(c.ID, "issue:10"); len(got) != 0 {
				t.Errorf("committed depends_on = %v, want rolled back (empty)", got)
			}
			if containsRef(st.Rollup.Eligible, "issue:10") {
				t.Errorf("rollup eligible = %v: a newly blocked item must never be reported eligible", st.Rollup.Eligible)
			}
			if st.NextAction.IssueRef == "issue:10" {
				t.Errorf("next_action = %+v names the held item", st.NextAction)
			}
			if st.DependencyRefresh == nil || st.DependencyRefresh.Committed || len(st.DependencyRefresh.Warnings) == 0 {
				t.Fatalf("dependency_refresh = %+v, want uncommitted with a warning", st.DependencyRefresh)
			}
			if name == "audit" && !strings.Contains(st.DependencyRefresh.Warnings[0], categoryCampaignDependenciesRefreshed) {
				t.Errorf("warning %q does not name the missing audit row", st.DependencyRefresh.Warnings[0])
			}
			assertHeld := func(phase string, st refreshStatusBody) {
				t.Helper()
				if containsRef(st.Rollup.Eligible, "issue:10") || st.NextAction.IssueRef == "issue:10" {
					t.Errorf("%s: issue:10 reported eligible (rollup %v, next_action %+v) while its blocker is unrecorded", phase, st.Rollup.Eligible, st.NextAction)
				}
				if st.DependencyRefresh == nil || st.DependencyRefresh.Committed || len(st.DependencyRefresh.Added) != 1 {
					t.Fatalf("%s: dependency_refresh = %+v, want the uncommitted hold carried", phase, st.DependencyRefresh)
				}
				if name == "audit" && !strings.Contains(strings.Join(st.DependencyRefresh.Warnings, " | "), categoryCampaignDependenciesRefreshed) {
					t.Errorf("%s: warnings %v do not name the missing audit row", phase, st.DependencyRefresh.Warnings)
				}
			}
			// Polls inside the interval are skipped by the gate, not re-observed.
			for i := 0; i < 2; i++ {
				st = mustStatus(t, h.s, c.ID)
				if st.DependencyRefresh == nil || st.DependencyRefresh.SkippedReason != refreshSkippedMinInterval {
					t.Fatalf("poll %d inside the interval: dependency_refresh = %+v, want min_interval", i, st.DependencyRefresh)
				}
				assertHeld("skipped poll", st)
			}
			// A tracker read failure observed nothing, so the hold stays.
			h.clk.advance(campaignDependencyRefreshMinInterval)
			h.res.err = errors.New("tracker down")
			st = mustStatus(t, h.s, c.ID)
			if !st.DependencyRefresh.Performed {
				t.Fatalf("forge-error poll did not read the tracker: %+v", st.DependencyRefresh)
			}
			assertHeld("forge-error poll", st)
			// The record recovers: the next refresh commits and the hold clears.
			h.res.err = nil
			h.rr.appendErr = nil
			h.s.cfg.AuditRepo = h.aud
			h.clk.advance(campaignDependencyRefreshMinInterval)
			st = mustStatus(t, h.s, c.ID)
			if got := h.itemDeps(c.ID, "issue:10"); !containsRef(got, "issue:11") {
				t.Fatalf("after recovery committed depends_on = %v, want issue:11", got)
			}
			if !st.DependencyRefresh.Committed || containsRef(st.Rollup.Eligible, "issue:10") {
				t.Errorf("after recovery: dependency_refresh %+v rollup %v", st.DependencyRefresh, st.Rollup.Eligible)
			}
			st = mustStatus(t, h.s, c.ID)
			if len(st.DependencyRefresh.Warnings) != 0 || len(st.DependencyRefresh.Added) != 0 {
				t.Errorf("recorded hold still carried: %+v", st.DependencyRefresh)
			}
			if containsRef(st.Rollup.Eligible, "issue:10") {
				t.Errorf("issue:10 eligible after its blocker was recorded: %v", st.Rollup.Eligible)
			}
		})
	}
}

// TestStatus_RelistFailureStillHolds: the refresh COMMITTED an edge but the
// status handler's re-list failed — the response holds the committed edge on
// the stale slice rather than reporting the newly blocked item eligible.
func TestStatus_RelistFailureStillHolds(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
	h.res.deps[10] = []int{11}
	h.rr.snapshot = true                          // the read's list must not alias the committed write
	h.repo.itemsErrOnCall = h.repo.itemsCalls + 2 // the read's own list, then the post-refresh re-list
	st := mustStatus(t, h.s, c.ID)
	if got := h.itemDeps(c.ID, "issue:10"); !containsRef(got, "issue:11") {
		t.Fatalf("committed depends_on = %v, want issue:11", got)
	}
	if st.DependencyRefresh == nil || !st.DependencyRefresh.Committed {
		t.Fatalf("dependency_refresh = %+v, want committed", st.DependencyRefresh)
	}
	if containsRef(st.Rollup.Eligible, "issue:10") || st.NextAction.IssueRef == "issue:10" {
		t.Errorf("issue:10 reported eligible (rollup %v, next_action %+v) despite its committed blocker", st.Rollup.Eligible, st.NextAction)
	}
}

// TestRefreshAccess_LookupErrorsDeny: every error on the access predicate's
// three lookups — the campaign's account, the repo filter, the visibility
// answer — DENIES the refresh (fail closed) with zero tracker reads and zero
// writes. Each row's other lookups allow, so the error is the only denial.
func TestRefreshAccess_LookupErrorsDeny(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg   Config
		setup func(*refreshHarness)
		want  string
	}{
		"account_lookup": {
			setup: func(h *refreshHarness) { h.rr.accountErr = errors.New("db down") },
			want:  refreshSkippedForeign,
		},
		"repo_filter": {
			cfg: Config{
				AccountRoles:   fakeAccountRoles{err: errors.New("roles down")},
				RepoVisibility: newFakeRepoVisibility(map[string]bool{"kuhlman-labs/fishhawk": true}),
			},
			want: refreshSkippedRepoInvisible,
		},
		"visibility": {
			cfg: Config{
				AccountRoles:   fakeAccountRoles{role: account.RoleMember},
				RepoVisibility: &fakeRepoVisibility{visible: map[string]bool{"kuhlman-labs/fishhawk": true}, err: errors.New("mirror down")},
			},
			want: refreshSkippedRepoInvisible,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRefreshHarness(t, tc.cfg)
			if tc.setup != nil {
				tc.setup(h)
			}
			c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
			h.res.deps[10] = []int{11}
			ctx := context.WithValue(context.Background(), ctxKeyIdentity, memberIdentity())
			items, _ := h.repo.ListCampaignItemsForCampaign(ctx, c.ID)
			res := h.s.refreshCampaignDependenciesForStatus(ctx, c, items)
			if res == nil || res.SkippedReason != tc.want {
				t.Errorf("refresh = %+v, want skipped %s", res, tc.want)
			}
			if h.res.callCount() != 0 || h.rr.appendCount() != 0 || h.aud.count(categoryCampaignDependenciesRefreshed) != 0 {
				t.Errorf("lookup error drove resolver %d append %d audit %d, want 0/0/0",
					h.res.callCount(), h.rr.appendCount(), h.aud.count(categoryCampaignDependenciesRefreshed))
			}
		})
	}
}

// TestRefresh_FlagOffNoForgeRead: a deployment without the flag makes no forge
// read on either trigger and omits the block.
func TestRefresh_FlagOffNoForgeRead(t *testing.T) {
	h := newRefreshHarness(t, Config{})
	h.s.cfg.CampaignDependencyRefresh = false
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
	h.res.deps[10] = []int{11}
	st := mustStatus(t, h.s, c.ID)
	if st.DependencyRefresh != nil || h.res.callCount() != 0 {
		t.Errorf("flag off: block %+v, resolver calls %d", st.DependencyRefresh, h.res.callCount())
	}
	items, _ := h.repo.ListCampaignItemsForCampaign(context.Background(), c.ID)
	if res, err := h.s.refreshCampaignDependencies(context.Background(), c, items, dependencyRefreshOpts{trigger: refreshTriggerStart}); err != nil || res.Performed || h.res.callCount() != 0 {
		t.Errorf("flag off core: %+v %v calls %d", res, err, h.res.callCount())
	}
}

// --- start trigger ---

func newRefreshStartServer(t *testing.T) (*Server, *refreshRepo, *fakeRepo, *categoryFailingAudit, *appendResolver) {
	t.Helper()
	crepo := newFakeCampaignRepo()
	s, rrepo, _ := newCampaignStartServer(t, crepo)
	aud := &categoryFailingAudit{}
	s.cfg.AuditRepo = aud
	rr := &refreshRepo{fakeCampaignRepo: crepo}
	s.cfg.CampaignRepo = rr
	s.cfg.CampaignDependencyRefresh = true
	res := &appendResolver{name: workmgmt.Default().Provider, deps: map[int][]int{}, autonomy: map[int]string{}, satisfied: map[int]bool{}}
	workmgmt.Register(res)
	return s, rr, rrepo, aud, res
}

func runCount(r *fakeRepo) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

const startP = `{"issue_ref":"issue:10","workflow_id":"feature_change","runner_kind":"local"}`

// TestStart_RefreshesTargetBeforeEligibility: with NO prior status poll, the
// start verb observes a post-start blocked-by and refuses item_not_eligible
// naming it (CF1-c vehicle).
func TestStart_RefreshesTargetBeforeEligibility(t *testing.T) {
	s, rr, rrepo, aud, res := newRefreshStartServer(t)
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{
		cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending),
	})
	res.deps[10] = []int{11}
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "item_not_eligible" {
		t.Fatalf("start = %d %s, want 409 item_not_eligible", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "issue:11") {
		t.Errorf("refusal does not name the blocker: %s", w.Body.String())
	}
	if runCount(rrepo) != 0 {
		t.Errorf("runs minted = %d, want 0", runCount(rrepo))
	}
	if n := aud.count(categoryCampaignDependenciesRefreshed); n != 1 {
		t.Errorf("refreshed rows = %d, want 1", n)
	}
	if res.callCount() != 1 || len(res.asked[0]) != 1 || res.asked[0][0] != "issue:10" {
		t.Errorf("resolver asked %v, want the target only", res.asked)
	}
}

// TestStart_AuditFailureFailsClosed: an unrecordable change starts nothing.
// The blocker is an already-SUCCEEDED member so the item would stay eligible:
// only the audit control decides the outcome (CF1-d vehicle).
func TestStart_AuditFailureFailsClosed(t *testing.T) {
	s, rr, rrepo, aud, res := newRefreshStartServer(t)
	aud.fail = categoryCampaignDependenciesRefreshed
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{
		cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStateSucceeded),
	})
	res.deps[10] = []int{11}
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusServiceUnavailable || decodeCampaignError(t, w) != "campaign_dependency_audit_failed" {
		t.Fatalf("start = %d %s, want 503 campaign_dependency_audit_failed", w.Code, w.Body.String())
	}
	if runCount(rrepo) != 0 {
		t.Errorf("runs minted = %d, want 0", runCount(rrepo))
	}
	items, _ := rr.ListCampaignItemsForCampaign(context.Background(), c.ID)
	if items[0].IssueRef != "issue:10" || len(items[0].DependsOn) != 0 || items[0].State != campaign.ItemStatePending {
		t.Errorf("item = %+v, want untouched", items[0])
	}
}

// TestStart_PersistFailureFailsClosed: any other write failure also starts
// nothing.
func TestStart_PersistFailureFailsClosed(t *testing.T) {
	s, rr, rrepo, _, res := newRefreshStartServer(t)
	rr.appendErr = errors.New("db down")
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{
		cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStateSucceeded),
	})
	res.deps[10] = []int{11}
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusServiceUnavailable || runCount(rrepo) != 0 {
		t.Fatalf("start = %d %s runs %d, want 503 and no run", w.Code, w.Body.String(), runCount(rrepo))
	}
}

// TestStart_ForgeFailureFailsClosed: an unreadable tracker starts nothing.
func TestStart_ForgeFailureFailsClosed(t *testing.T) {
	s, rr, rrepo, _, res := newRefreshStartServer(t)
	res.err = errors.New("tracker down")
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{cItem("issue:10", nil, campaign.ItemStatePending)})
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusBadGateway || decodeCampaignError(t, w) != "campaign_dependency_refresh_failed" {
		t.Fatalf("start = %d %s, want 502 campaign_dependency_refresh_failed", w.Code, w.Body.String())
	}
	if runCount(rrepo) != 0 {
		t.Errorf("runs minted = %d, want 0", runCount(rrepo))
	}
}

// TestStart_ForeignAccountSkipsRefresh: a foreign-account start drives zero
// resolver calls and zero writes, and the start behaves as before (condition
// 1's counterfactual vehicle: without the gate the refresh holds the item).
func TestStart_ForeignAccountSkipsRefresh(t *testing.T) {
	s, rr, _, aud, res := newRefreshStartServer(t)
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{
		cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending),
	})
	rr.accounts[c.ID] = uuid.NewString()
	res.deps[10] = []int{11}
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusCreated {
		t.Fatalf("start = %d %s, want 201 (unchanged start behaviour)", w.Code, w.Body.String())
	}
	if res.callCount() != 0 || rr.appendCount() != 0 || aud.count(categoryCampaignDependenciesRefreshed) != 0 {
		t.Errorf("foreign start drove resolver %d append %d audit %d, want 0/0/0",
			res.callCount(), rr.appendCount(), aud.count(categoryCampaignDependenciesRefreshed))
	}
}

// TestStart_RepoInvisibleSkipsRefresh: a SAME-account caller who cannot see
// the repo drives zero resolver calls, zero writes and zero audit rows, and the
// start behaves as before (condition 1's visibility half; without the check
// the refresh holds the item and the start refuses 409).
func TestStart_RepoInvisibleSkipsRefresh(t *testing.T) {
	s, rr, _, aud, res := newRefreshStartServer(t)
	s.cfg.AccountRoles = fakeAccountRoles{role: account.RoleMember}
	vis := newFakeRepoVisibility(map[string]bool{})
	s.cfg.RepoVisibility = vis
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{
		cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending),
	})
	rr.accounts[c.ID] = testOperatorAccountID
	res.deps[10] = []int{11}
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusCreated {
		t.Fatalf("start = %d %s, want 201 (unchanged start behaviour)", w.Code, w.Body.String())
	}
	if vis.callCount() == 0 {
		t.Fatal("the visibility mirror was never asked: the fixture does not exercise the predicate")
	}
	if res.callCount() != 0 || rr.appendCount() != 0 || aud.count(categoryCampaignDependenciesRefreshed) != 0 {
		t.Errorf("repo-invisible start drove resolver %d append %d audit %d, want 0/0/0",
			res.callCount(), rr.appendCount(), aud.count(categoryCampaignDependenciesRefreshed))
	}
}

// TestStart_NoOpRefreshMakesNoRepositoryCall: nothing new observed → no
// repository call, no audit row, the start proceeds (condition 4).
func TestStart_NoOpRefreshMakesNoRepositoryCall(t *testing.T) {
	s, rr, _, aud, res := newRefreshStartServer(t)
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{
		cItem("issue:10", []string{"issue:11"}, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStateSucceeded),
	})
	res.deps[10] = []int{11}
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusCreated {
		t.Fatalf("start = %d %s, want 201", w.Code, w.Body.String())
	}
	if res.callCount() != 1 || rr.appendCount() != 0 || aud.count(categoryCampaignDependenciesRefreshed) != 0 {
		t.Errorf("no-op: resolver %d append %d audit %d, want 1/0/0", res.callCount(), rr.appendCount(), aud.count(categoryCampaignDependenciesRefreshed))
	}
}

// TestStart_RelistFailureStillHolds: the edge committed but the re-list
// failed — the gate holds the observed edge on the stale slice.
func TestStart_RelistFailureStillHolds(t *testing.T) {
	s, rr, rrepo, _, res := newRefreshStartServer(t)
	c := rr.seedCampaignWithItems("kuhlman-labs/fishhawk", "", []*campaign.Item{
		cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending),
	})
	res.deps[10] = []int{11}
	rr.snapshot = true                    // the start's list must not alias the committed write
	rr.itemsErrOnCall = rr.itemsCalls + 2 // the start's own list, then the post-refresh re-list
	w := postStartItemRun(t, s, c.ID, startP)
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "item_not_eligible" || runCount(rrepo) != 0 {
		t.Fatalf("start = %d %s runs %d, want 409 item_not_eligible and no run", w.Code, w.Body.String(), runCount(rrepo))
	}
}

// TestCampaignAppendRefresh_EndToEnd_PG is the cross-layer test: the real
// append route → handler → campaign.PlanAppend → the Postgres repository →
// GET status → the dependency refresh (through the same repository
// transaction) → the rollup, crossing request payload, domain planner,
// persistence and render.
func TestCampaignAppendRefresh_EndToEnd_PG(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)
	c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{Repo: "kuhlman-labs/fishhawk", PausePolicy: campaign.PausePolicyPauseCampaign})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if _, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:10", Position: 0}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	h := newRefreshHarness(t, Config{})
	h.s.cfg.CampaignRepo = repo

	if w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity()); w.Code != http.StatusCreated {
		t.Fatalf("append = %d %s", w.Code, w.Body.String())
	}
	st := mustStatus(t, h.s, c.ID)
	if !containsRef(st.Rollup.Eligible, "issue:20") || !containsRef(st.Rollup.Eligible, "issue:10") {
		t.Fatalf("after append both should be eligible: %+v", st.Rollup)
	}

	// The tracker gains issue:20 blocked-by issue:10 after the append.
	h.res.deps[20] = []int{10}
	h.clk.advance(campaignDependencyRefreshMinInterval)
	st = mustStatus(t, h.s, c.ID)
	if containsRef(st.Rollup.Eligible, "issue:20") {
		t.Fatalf("issue:20 still eligible after the tracker added blocked-by issue:10: %+v", st.Rollup)
	}
	items, err := repo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.IssueRef == "issue:20" && (len(it.DependsOn) != 1 || it.DependsOn[0] != "issue:10") {
			t.Errorf("committed depends_on = %v, want [issue:10]", it.DependsOn)
		}
	}
	if n := h.aud.count(categoryCampaignDependenciesRefreshed); n != 1 {
		t.Errorf("refreshed rows = %d, want 1", n)
	}
	if n := h.aud.count(categoryCampaignItemsAppended); n != 1 {
		t.Errorf("appended rows = %d, want 1", n)
	}
}
