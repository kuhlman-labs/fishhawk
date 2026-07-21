package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// --- in-memory campaign repository fake ---

// fakeCampaignRepo is an in-memory campaign.Repository for handler tests.
// The Postgres adapter is exercised in backend/internal/campaign/postgres_test.go
// (and by the cross-boundary e2e below), so this fake only needs to satisfy
// the contract well enough that handler logic gets coverage. It embeds
// BaseFake so it doesn't have to stub the transition/run-linkage methods the
// REST handlers never call.
type fakeCampaignRepo struct {
	campaign.BaseFake
	mu         sync.Mutex
	campaigns  map[uuid.UUID]*campaign.Campaign
	itemsByCmp map[uuid.UUID][]*campaign.Item

	// error injections so the 5xx surfaces are reachable.
	createErr    error
	getErr       error
	getIdempErr  error
	listErr      error
	itemsErr     error
	transCmpErr  error
	transItemErr error
	setRunErr    error
	restartErr   error

	// itemsErrOnCall, when non-zero, fails ListCampaignItemsForCampaign on
	// exactly that 1-based call number (others succeed), so a test can exercise
	// reconcile's re-list-after-settle swallow path without also failing the
	// read's initial list. itemsCalls counts the invocations it gates on.
	itemsErrOnCall int
	itemsCalls     int

	// lastListFilter records the most recent ListCampaigns filter so handler
	// tests can assert the account-scope wire-up (ADR-057 / #1830).
	lastListFilter campaign.ListCampaignsFilter

	// accounts maps campaign id -> workspace account for the AccountGetter
	// capability override below; a campaign absent from the map is
	// untenanted ("").
	accounts map[uuid.UUID]string

	// call counters so idempotency tests can assert a no-op re-poll performs no
	// further mutation.
	transItemCalls int
	transCmpCalls  int
	setRunCalls    int
	restartCalls   int
	settleOOBCalls int
}

func newFakeCampaignRepo() *fakeCampaignRepo {
	return &fakeCampaignRepo{
		campaigns:  map[uuid.UUID]*campaign.Campaign{},
		itemsByCmp: map[uuid.UUID][]*campaign.Item{},
		accounts:   map[uuid.UUID]string{},
	}
}

func (f *fakeCampaignRepo) CreateCampaign(_ context.Context, p campaign.CreateCampaignParams) (*campaign.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	now := time.Now().UTC()
	c := &campaign.Campaign{
		ID:             uuid.New(),
		Repo:           p.Repo,
		EpicRef:        p.EpicRef,
		State:          campaign.StatePending,
		PausePolicy:    p.PausePolicy,
		OperatorAgent:  p.OperatorAgent,
		IdempotencyKey: p.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	f.campaigns[c.ID] = c
	return c, nil
}

// GetCampaignByIdempotencyKey scans for a campaign matching (repo, key),
// mirroring the Postgres adapter's ErrNotFound-on-miss contract. getIdempErr
// injects a non-NotFound error so the handler's 500 lookup-failed branch is
// reachable.
func (f *fakeCampaignRepo) GetCampaignByIdempotencyKey(_ context.Context, repo, key string) (*campaign.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getIdempErr != nil {
		return nil, f.getIdempErr
	}
	for _, c := range f.campaigns {
		if c.Repo == repo && c.IdempotencyKey != nil && *c.IdempotencyKey == key {
			return c, nil
		}
	}
	return nil, campaign.ErrNotFound
}

// TransitionCampaign moves a campaign to the target state, mirroring the
// Postgres adapter's idempotent same-state + InvalidTransitionError contract.
func (f *fakeCampaignRepo) TransitionCampaign(_ context.Context, id uuid.UUID, to campaign.State) (*campaign.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transCmpCalls++
	if f.transCmpErr != nil {
		return nil, f.transCmpErr
	}
	c, ok := f.campaigns[id]
	if !ok {
		return nil, campaign.ErrNotFound
	}
	if !campaign.ValidCampaignTransition(c.State, to) {
		return nil, campaign.InvalidTransitionError{Kind: "campaign", From: string(c.State), To: string(to)}
	}
	c.State = to
	c.UpdatedAt = time.Now().UTC()
	return c, nil
}

// TransitionCampaignItem moves an item (found by id across all campaigns) to
// the target state with the same idempotent/InvalidTransition contract.
func (f *fakeCampaignRepo) TransitionCampaignItem(_ context.Context, id uuid.UUID, to campaign.ItemState) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transItemCalls++
	if f.transItemErr != nil {
		return nil, f.transItemErr
	}
	for _, items := range f.itemsByCmp {
		for _, it := range items {
			if it.ID != id {
				continue
			}
			if !campaign.ValidCampaignItemTransition(it.State, to) {
				return nil, campaign.InvalidTransitionError{Kind: "campaign_item", From: string(it.State), To: string(to)}
			}
			it.State = to
			it.UpdatedAt = time.Now().UTC()
			return it, nil
		}
	}
	return nil, campaign.ErrNotFound
}

// SetCampaignItemRun links (or, with nil, unlinks) an item to its run, mirroring
// the Postgres adapter's run-linkage write so the start handler's link step is
// exercised.
func (f *fakeCampaignRepo) SetCampaignItemRun(_ context.Context, itemID uuid.UUID, runID *uuid.UUID) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setRunCalls++
	if f.setRunErr != nil {
		return nil, f.setRunErr
	}
	for _, items := range f.itemsByCmp {
		for _, it := range items {
			if it.ID != itemID {
				continue
			}
			it.RunID = runID
			it.UpdatedAt = time.Now().UTC()
			return it, nil
		}
	}
	return nil, campaign.ErrNotFound
}

// RestartCampaignItem resets a restartable-terminal item (cancelled or failed)
// back to pending and clears its run link, mirroring the Postgres adapter's
// InvalidTransitionError/ErrNotFound contract so the start handler's restart
// path is exercised. restartErr injects a non-terminal error so the 500 branch
// is reachable; restartCalls counts invocations for the audit/mutation asserts.
func (f *fakeCampaignRepo) RestartCampaignItem(_ context.Context, id uuid.UUID) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restartCalls++
	if f.restartErr != nil {
		return nil, f.restartErr
	}
	for _, items := range f.itemsByCmp {
		for _, it := range items {
			if it.ID != id {
				continue
			}
			if it.State != campaign.ItemStateCancelled && it.State != campaign.ItemStateFailed {
				return nil, campaign.InvalidTransitionError{Kind: "campaign_item", From: string(it.State), To: string(campaign.ItemStatePending)}
			}
			it.State = campaign.ItemStatePending
			it.RunID = nil
			it.UpdatedAt = time.Now().UTC()
			return it, nil
		}
	}
	return nil, campaign.ErrNotFound
}

// SettleCampaignItemOutOfBand settles a terminal-non-succeeded item (cancelled
// or failed) to succeeded while RETAINING its run link, mirroring the Postgres
// adapter's guard-bypassing contract (#2029) so reconcile-on-read's class-B
// (out-of-band terminal) settle path is exercised. Any other from-state is
// rejected InvalidTransitionError; settleOOBCalls counts invocations for the
// mutation asserts.
func (f *fakeCampaignRepo) SettleCampaignItemOutOfBand(_ context.Context, id uuid.UUID) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settleOOBCalls++
	for _, items := range f.itemsByCmp {
		for _, it := range items {
			if it.ID != id {
				continue
			}
			if it.State != campaign.ItemStateCancelled && it.State != campaign.ItemStateFailed {
				return nil, campaign.InvalidTransitionError{Kind: "campaign_item", From: string(it.State), To: string(campaign.ItemStateSucceeded)}
			}
			it.State = campaign.ItemStateSucceeded
			// Run link RETAINED (unlike RestartCampaignItem, which clears it).
			it.UpdatedAt = time.Now().UTC()
			return it, nil
		}
	}
	return nil, campaign.ErrNotFound
}

func (f *fakeCampaignRepo) CreateCampaignItem(_ context.Context, p campaign.CreateCampaignItemParams) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	it := &campaign.Item{
		ID:         uuid.New(),
		CampaignID: p.CampaignID,
		IssueRef:   p.IssueRef,
		DependsOn:  p.DependsOn,
		State:      campaign.ItemStatePending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	f.itemsByCmp[p.CampaignID] = append(f.itemsByCmp[p.CampaignID], it)
	return it, nil
}

// GetCampaignAccountID overrides BaseFake's ErrNotFound no-op so handler
// tests can exercise the GET /v0/campaigns/{id} ownership check
// (ADR-057 / #1830): a campaign seeded into the accounts map is tenanted;
// everything else is untenanted ("").
func (f *fakeCampaignRepo) GetCampaignAccountID(_ context.Context, id uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.campaigns[id]; !ok {
		return "", campaign.ErrNotFound
	}
	return f.accounts[id], nil
}

func (f *fakeCampaignRepo) GetCampaign(_ context.Context, id uuid.UUID) (*campaign.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.campaigns[id]
	if !ok {
		return nil, campaign.ErrNotFound
	}
	return c, nil
}

func (f *fakeCampaignRepo) ListCampaigns(_ context.Context, fil campaign.ListCampaignsFilter) ([]*campaign.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastListFilter = fil
	if f.listErr != nil {
		return nil, f.listErr
	}
	var matched []*campaign.Campaign
	for _, c := range f.campaigns {
		if fil.Repo != "" && c.Repo != fil.Repo {
			continue
		}
		if fil.State != "" && string(c.State) != fil.State {
			continue
		}
		matched = append(matched, c)
	}
	// Deterministic order: created_at DESC, id DESC (matches the SQL contract).
	for i := 0; i < len(matched); i++ {
		for j := i + 1; j < len(matched); j++ {
			if matched[j].CreatedAt.After(matched[i].CreatedAt) ||
				(matched[j].CreatedAt.Equal(matched[i].CreatedAt) && matched[j].ID.String() > matched[i].ID.String()) {
				matched[i], matched[j] = matched[j], matched[i]
			}
		}
	}
	if fil.Offset >= len(matched) {
		return nil, nil
	}
	end := fil.Offset + fil.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[fil.Offset:end], nil
}

func (f *fakeCampaignRepo) ListCampaignItemsForCampaign(_ context.Context, id uuid.UUID) ([]*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.itemsCalls++
	if f.itemsErr != nil {
		return nil, f.itemsErr
	}
	if f.itemsErrOnCall != 0 && f.itemsCalls == f.itemsErrOnCall {
		return nil, errInjected
	}
	out := make([]*campaign.Item, len(f.itemsByCmp[id]))
	copy(out, f.itemsByCmp[id])
	return out, nil
}

// seedCampaignWithItems stores a campaign and its items directly, bypassing
// the assemble path, for the read-handler tests.
func (f *fakeCampaignRepo) seedCampaignWithItems(repo, epicRef string, items []*campaign.Item) *campaign.Campaign {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	c := &campaign.Campaign{
		ID:        uuid.New(),
		Repo:      repo,
		EpicRef:   epicRef,
		State:     campaign.StateRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}
	f.campaigns[c.ID] = c
	for _, it := range items {
		it.CampaignID = c.ID
		if it.ID == uuid.Nil {
			it.ID = uuid.New()
		}
	}
	f.itemsByCmp[c.ID] = items
	return c
}

// --- fake epic-children provider ---

// fakeEpicProvider is a workmgmt.Provider that ALSO implements
// EpicChildrenQuerier, so the create handler's provider resolution + epic
// query seam is exercised. It records the request it received and returns a
// canned result (or a configured error).
type fakeEpicProvider struct {
	name     string
	result   *workmgmt.EpicChildrenResult
	queryErr error
	called   bool
	captured workmgmt.EpicChildrenRequest
}

func (f *fakeEpicProvider) Name() string { return f.name }

func (f *fakeEpicProvider) File(_ context.Context, _ workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	return &workmgmt.CreatedItem{Provider: f.name}, nil
}

func (f *fakeEpicProvider) EpicChildren(_ context.Context, req workmgmt.EpicChildrenRequest) (*workmgmt.EpicChildrenResult, error) {
	f.called = true
	f.captured = req
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.result, nil
}

// registerEpicProvider registers p under the default conventions' provider id
// so the handler's workmgmt.Get resolves it. The registry is process-global
// with no deregister, so each test re-registers a fresh fake.
func registerEpicProvider(t *testing.T, p *fakeEpicProvider) {
	t.Helper()
	if p.name == "" {
		p.name = workmgmt.Default().Provider
	}
	workmgmt.Register(p)
}

// smallDAG is the canonical two-item DAG used across the create tests:
// issue 100 has no deps (wave 0, eligible), issue 101 depends on 100
// (wave 1, blocked until 100 succeeds).
func smallDAG() *workmgmt.EpicChildrenResult {
	return &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 100, Title: "first"}, {Number: 101, Title: "second"}},
		Edges:    []workmgmt.DependsEdge{{From: 101, To: 100}},
	}
}

// recordingInstallGitHubClient builds a *githubclient.Client whose
// installation endpoint records the (owner, name) it was queried with, so a
// test can assert the create handler called GetRepoInstallation with the
// request's RepoRef.
type installRecorder struct {
	mu    sync.Mutex
	owner string
	name  string
	hits  int
}

func recordingInstallGitHubClient(t *testing.T, installID int64, rec *installRecorder) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.owner = r.PathValue("owner")
		rec.name = r.PathValue("name")
		rec.hits++
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d}`, installID)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// postCampaign POSTs a create body to handleCreateCampaign with an operator
// identity (scope bypass) and returns the recorder.
func postCampaign(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateCampaign(w, withAuth(req))
	return w
}

// postCampaignWithKey POSTs a create body carrying an Idempotency-Key header
// (E25.13), otherwise identical to postCampaign.
func postCampaignWithKey(t *testing.T, s *Server, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	s.handleCreateCampaign(w, withAuth(req))
	return w
}

func decodeCampaignError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, w.Body.String())
	}
	return env.Error.Code
}

// --- create handler tests ---

// TestCreateCampaign_CrossBoundary_E2E drives the full surface end-to-end
// against a real Postgres CampaignRepo: request payload -> GitHub install
// resolution -> workmgmt provider EpicChildren -> campaign.Assemble ->
// Postgres persistence -> GET /status render. A per-layer unit would miss a
// broken seam (#618); this asserts the rollup partition AND next_action
// cross the wire intact.
func TestCreateCampaign_CrossBoundary_E2E(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)

	gh := recordingInstallGitHubClient(t, 7788, &installRecorder{})
	s := New(Config{CampaignRepo: repo, GitHub: gh})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created campaign: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("created campaign id is zero")
	}
	if created.Repo != "kuhlman-labs/fishhawk" || created.EpicRef != "issue:99" {
		t.Errorf("created = %+v, want repo/epic_ref echoed", created)
	}
	if created.State != string(campaign.StatePending) {
		t.Errorf("state = %q, want pending", created.State)
	}

	// GET /status and assert the partition + next_action crossed the wire.
	statusReq := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+created.ID.String()+"/status", nil)
	statusReq.SetPathValue("campaign_id", created.ID.String())
	sw := httptest.NewRecorder()
	s.handleGetCampaignStatus(sw, withAuth(statusReq))
	if sw.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", sw.Code, sw.Body.String())
	}
	var status struct {
		Campaign   campaignResponse          `json:"campaign"`
		Items      []campaignItemResponse    `json:"items"`
		Rollup     campaignRollupPayload     `json:"rollup"`
		NextAction campaignNextActionPayload `json:"next_action"`
	}
	if err := json.Unmarshal(sw.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, sw.Body.String())
	}
	if len(status.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(status.Items))
	}
	// issue 100 has no deps -> eligible; issue 101 depends on 100 -> blocked.
	if len(status.Rollup.Eligible) != 1 || status.Rollup.Eligible[0] != "issue:100" {
		t.Errorf("rollup.Eligible = %v, want [issue:100]", status.Rollup.Eligible)
	}
	if len(status.Rollup.Blocked) != 1 || status.Rollup.Blocked[0] != "issue:101" {
		t.Errorf("rollup.Blocked = %v, want [issue:101]", status.Rollup.Blocked)
	}
	if status.NextAction.Action != "start_run" || status.NextAction.IssueRef != "issue:100" {
		t.Errorf("next_action = %+v, want start_run issue:100", status.NextAction)
	}
	// The blocked item carries its depends_on edge across the wire.
	var blocked *campaignItemResponse
	for i := range status.Items {
		if status.Items[i].IssueRef == "issue:101" {
			blocked = &status.Items[i]
		}
	}
	if blocked == nil || len(blocked.DependsOn) != 1 || blocked.DependsOn[0] != "issue:100" {
		t.Errorf("issue:101 depends_on = %+v, want [issue:100]", blocked)
	}
}

// threeChildDAG is a three-item fixture for the subset tests: 100 has no deps
// (wave 0), 101 and 102 each depend on 100 (wave 1).
func threeChildDAG() *workmgmt.EpicChildrenResult {
	return &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{
			{Number: 100, Title: "first"},
			{Number: 101, Title: "second"},
			{Number: 102, Title: "third"},
		},
		Edges: []workmgmt.DependsEdge{
			{From: 101, To: 100},
			{From: 102, To: 100},
		},
	}
}

// TestCreateCampaign_ItemsSubset_CrossBoundary_E2E drives the subset filter
// (#2003) end-to-end: a POST naming a subset of the epic's children assembles
// the DAG over ONLY those items (request payload -> FilterToSubset -> Assemble
// -> Postgres -> GET /status render).
func TestCreateCampaign_ItemsSubset_CrossBoundary_E2E(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	fp := &fakeEpicProvider{result: threeChildDAG()}
	registerEpicProvider(t, fp)

	gh := recordingInstallGitHubClient(t, 7788, &installRecorder{})
	s := New(Config{CampaignRepo: repo, GitHub: gh})

	// Scope to {100, 101}, dropping 102 and its edge.
	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","items":["issue:100","issue:101"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created campaign: %v", err)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+created.ID.String()+"/status", nil)
	statusReq.SetPathValue("campaign_id", created.ID.String())
	sw := httptest.NewRecorder()
	s.handleGetCampaignStatus(sw, withAuth(statusReq))
	if sw.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", sw.Code, sw.Body.String())
	}
	var status struct {
		Items []campaignItemResponse `json:"items"`
	}
	if err := json.Unmarshal(sw.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, sw.Body.String())
	}
	if len(status.Items) != 2 {
		t.Fatalf("items = %d, want 2 (subset {100,101})", len(status.Items))
	}
	refs := map[string]bool{}
	for _, it := range status.Items {
		refs[it.IssueRef] = true
	}
	if !refs["issue:100"] || !refs["issue:101"] || refs["issue:102"] {
		t.Errorf("item refs = %v, want exactly {issue:100, issue:101}", refs)
	}
}

// TestCreateCampaign_ItemNotChild_422 is the fail-closed branch: a requested
// item that is not a child of the epic returns 422 campaign_item_not_child.
func TestCreateCampaign_ItemNotChild_422(t *testing.T) {
	fp := &fakeEpicProvider{result: threeChildDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: install skipped

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","items":["issue:100","issue:999"]}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_item_not_child" {
		t.Errorf("error code = %q, want campaign_item_not_child", code)
	}
}

// TestCreateCampaign_SubsetIncludedDependsOnExcluded_422 is the re-classified
// DroppedEdges path: including 101 (depends on 100) while excluding 100 makes
// 101's dependency dangling, surfaced as 422 campaign_dangling_dependency —
// reusing the existing mapping with no handler change.
func TestCreateCampaign_SubsetIncludedDependsOnExcluded_422(t *testing.T) {
	fp := &fakeEpicProvider{result: threeChildDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: install skipped

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","items":["issue:101"]}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_dangling_dependency" {
		t.Errorf("error code = %q, want campaign_dangling_dependency", code)
	}
}

// TestCreateCampaign_OmittedItems_SweepsAllChildren is the backward-compat
// regression guard: omitting items assembles the campaign over EVERY child,
// unchanged from the pre-subset behavior.
func TestCreateCampaign_OmittedItems_SweepsAllChildren(t *testing.T) {
	fp := &fakeEpicProvider{result: threeChildDAG()}
	registerEpicProvider(t, fp)
	repo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: repo}) // GitHub nil: install skipped

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created campaign: %v", err)
	}
	items, err := repo.ListCampaignItemsForCampaign(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("persisted items = %d, want 3 (all children swept)", len(items))
	}
}

// TestCreateCampaign_OperatorAgent_CrossBoundary_E2E is the slice-A cross-layer
// done-means for the campaign-level operator_agent override (E25.12): a POST
// carrying an operator_agent block flows payload -> domain -> JSONB persistence
// -> response, and GET /status echoes it back value-for-value (crossing the
// handler, campaign.Persist, the Postgres column, and rowToCampaign).
func TestCreateCampaign_OperatorAgent_CrossBoundary_E2E(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)

	gh := recordingInstallGitHubClient(t, 7799, &installRecorder{})
	s := New(Config{CampaignRepo: repo, GitHub: gh})

	override := `{"may_approve":"solo_low","must_page_human":["reviewer_reject"]}`
	body := `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","operator_agent":` + override + `}`
	w := postCampaign(t, s, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created campaign: %v", err)
	}
	if !jsonValueEqual(t, created.OperatorAgent, []byte(override)) {
		t.Errorf("created operator_agent = %s, want value-equal to %s", created.OperatorAgent, override)
	}

	// GET /status re-reads from the column and echoes the override.
	statusReq := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+created.ID.String()+"/status", nil)
	statusReq.SetPathValue("campaign_id", created.ID.String())
	sw := httptest.NewRecorder()
	s.handleGetCampaignStatus(sw, withAuth(statusReq))
	if sw.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", sw.Code, sw.Body.String())
	}
	var status struct {
		Campaign campaignResponse `json:"campaign"`
	}
	if err := json.Unmarshal(sw.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, sw.Body.String())
	}
	if !jsonValueEqual(t, status.Campaign.OperatorAgent, []byte(override)) {
		t.Errorf("status campaign operator_agent = %s, want value-equal to %s", status.Campaign.OperatorAgent, override)
	}
}

// TestCreateCampaign_Idempotency_Replay_E2E is the cross-boundary done-means
// for E25.13 (#1455): two POST /v0/campaigns with the SAME Idempotency-Key
// against a real Postgres CampaignRepo return the SAME campaign id, the second
// with HTTP 200 + idempotent:true, and EXACTLY ONE campaign row is persisted —
// duplicate suppression that fails if the dedup is a no-op. The replay also
// short-circuits before the GitHub install resolution.
func TestCreateCampaign_Idempotency_Replay_E2E(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)

	rec := &installRecorder{}
	gh := recordingInstallGitHubClient(t, 7788, rec)
	s := New(Config{CampaignRepo: repo, GitHub: gh})

	body := `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`

	// First POST: fresh create at 201, no idempotent flag.
	w1 := postCampaignWithKey(t, s, body, "campaign-key-1")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201 (body=%s)", w1.Code, w1.Body.String())
	}
	var first campaignResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if first.Idempotent {
		t.Error("first create carried idempotent:true, want false on a fresh create")
	}

	// Second POST same key: replay at 200 + idempotent:true, same id.
	w2 := postCampaignWithKey(t, s, body, "campaign-key-1")
	if w2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (body=%s)", w2.Code, w2.Body.String())
	}
	var second campaignResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("replay id = %s, want %s (same campaign)", second.ID, first.ID)
	}
	if !second.Idempotent {
		t.Error("replay missing idempotent:true")
	}

	// EXACTLY ONE campaign row for the repo — the duplicate was suppressed.
	rows, err := repo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{
		Repo:  "kuhlman-labs/fishhawk",
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("persisted campaign count = %d, want 1 (duplicate suppressed)", len(rows))
	}

	// The replay short-circuited before the GitHub install resolution: only the
	// first create hit it.
	rec.mu.Lock()
	hits := rec.hits
	rec.mu.Unlock()
	if hits != 1 {
		t.Errorf("install endpoint hits = %d, want 1 (replay must not do GitHub work)", hits)
	}
}

// TestCreateCampaign_Idempotency_FirstCall_201_NoFlag pins the ErrNotFound
// fall-through branch: the FIRST POST carrying a key (no prior campaign)
// creates at 201 and the response carries NO idempotent key (omitempty over
// false), so a fresh create is byte-identical to a keyless one.
func TestCreateCampaign_Idempotency_FirstCall_201_NoFlag(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: install skipped

	w := postCampaignWithKey(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`, "fresh-key")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["idempotent"]; present {
		t.Errorf("idempotent key present on a fresh create, want omitted (body=%s)", w.Body.String())
	}
}

// TestCreateCampaign_Idempotency_LookupError_500 covers the third idempotency
// branch: a non-NotFound error from GetCampaignByIdempotencyKey surfaces 500
// internal_error and never dispatches the provider.
func TestCreateCampaign_Idempotency_LookupError_500(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	repo := newFakeCampaignRepo()
	repo.getIdempErr = fmt.Errorf("boom")
	s := New(Config{CampaignRepo: repo})

	w := postCampaignWithKey(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`, "key-x")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
	if fp.called {
		t.Error("provider dispatched despite an idempotency lookup error")
	}
}

// TestCreateCampaign_NoOperatorAgent_Omitted is the unchanged-behavior pin: a
// create with no operator_agent yields a response with NO operator_agent key
// (omitempty over nil bytes), so existing campaigns and clients see byte-
// identical output to pre-E25.12.
func TestCreateCampaign_NoOperatorAgent_Omitted(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: install skipped

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["operator_agent"]; present {
		t.Errorf("operator_agent key present with no override, want omitted (body=%s)", w.Body.String())
	}
}

// TestCreateCampaign_OperatorAgent_Echoed_201 asserts a well-formed override is
// accepted (201) and echoed on the response — the happy path of the new
// validateOperatorAgent branch over the fake repo.
func TestCreateCampaign_OperatorAgent_Echoed_201(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: install skipped

	override := `{"may_retry":"infra_flake"}`
	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","operator_agent":`+override+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var c campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !jsonValueEqual(t, c.OperatorAgent, []byte(override)) {
		t.Errorf("operator_agent = %s, want value-equal to %s", c.OperatorAgent, override)
	}
}

// TestCreateCampaign_MalformedOperatorAgent_400 covers the validateOperatorAgent
// reject branch for a syntactically-valid but type-wrong block (a JSON string,
// not an operator-agent object): 400 validation_failed, before any provider
// dispatch.
func TestCreateCampaign_MalformedOperatorAgent_400(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","operator_agent":"not-an-object"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
	if fp.called {
		t.Error("provider dispatched despite a malformed operator_agent")
	}
}

// TestCreateCampaign_UnknownFieldOperatorAgent_400 covers the
// DisallowUnknownFields reject branch: an operator_agent block carrying an
// unrecognized knob is rejected 400, not stored opaquely.
func TestCreateCampaign_UnknownFieldOperatorAgent_400(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","operator_agent":{"bogus_knob":"x"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
	if fp.called {
		t.Error("provider dispatched despite an unknown operator_agent field")
	}
}

// TestCreateCampaign_NullOperatorAgent_201 pins the "JSON null is not an
// override" branch: an explicit `null` is treated as absent (201, no
// operator_agent echoed), never stored as the literal bytes "null".
func TestCreateCampaign_NullOperatorAgent_201(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","operator_agent":null}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["operator_agent"]; present {
		t.Errorf("operator_agent key present for a null override, want omitted (body=%s)", w.Body.String())
	}
}

// jsonValueEqual reports whether two raw JSON blobs are semantically equal,
// tolerating whitespace/key-order normalization (Postgres JSONB storage).
func jsonValueEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %q: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// TestCreateCampaign_ResolvesInstallation asserts the create handler CALLS
// GetRepoInstallation with the request's RepoRef — the install-resolution
// wiring required beyond the happy-path e2e fake.
func TestCreateCampaign_ResolvesInstallation(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)

	rec := &installRecorder{}
	gh := recordingInstallGitHubClient(t, 4242, rec)
	s := New(Config{CampaignRepo: newFakeCampaignRepo(), GitHub: gh})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.hits == 0 {
		t.Fatal("GetRepoInstallation was not called")
	}
	if rec.owner != "kuhlman-labs" || rec.name != "fishhawk" {
		t.Errorf("GetRepoInstallation RepoRef = %s/%s, want kuhlman-labs/fishhawk", rec.owner, rec.name)
	}
	// The resolved id reaches the provider's EpicChildren target.
	if fp.captured.Target.Scope != forge.FromGitHubInstallationID(4242) {
		t.Errorf("provider Target.Scope = %q, want scope for installation 4242", fp.captured.Target.Scope.Ref())
	}
	if fp.captured.Epic != "issue:99" {
		t.Errorf("provider Epic = %q, want issue:99", fp.captured.Epic)
	}
}

// TestCreateCampaign_NotInstalled_422 asserts the App-not-installed branch
// (404 -> ErrNotInstalled) surfaces a typed 422, not a 500/panic.
func TestCreateCampaign_NotInstalled_422(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)

	gh := newInstallationGitHubClient(t, 0, true) // 404 -> ErrNotInstalled
	s := New(Config{CampaignRepo: newFakeCampaignRepo(), GitHub: gh})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "repo_not_installed" {
		t.Errorf("code = %q, want repo_not_installed", code)
	}
	if fp.called {
		t.Error("provider EpicChildren dispatched despite unresolved installation")
	}
}

// TestCreateCampaign_InstallationError_502 asserts a transient
// (non-ErrNotInstalled) installation-resolution failure is surfaced as 502
// by the handler before any provider dispatch.
func TestCreateCampaign_InstallationError_502(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"server error"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	s := New(Config{CampaignRepo: newFakeCampaignRepo(), GitHub: gh})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "installation_resolution_failed" {
		t.Errorf("code = %q, want installation_resolution_failed", code)
	}
	if fp.called {
		t.Error("provider dispatched despite a resolution error")
	}
}

// TestCreateCampaign_Idempotency_Dedup_SameID is the enforced-dedup pin over
// the fake repo (E25.13 / #1455, superseding the prior "header ignored, not
// honoured" pin): two POSTs of the same {repo, epic_ref} WITH the same
// Idempotency-Key return the SAME campaign id — the first at 201, the replay at
// 200. A different key mints a distinct campaign. The Postgres-backed
// duplicate-suppression done-means lives in
// TestCreateCampaign_Idempotency_Replay_E2E.
func TestCreateCampaign_Idempotency_Dedup_SameID(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: install resolution skipped

	body := `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`
	post := func(key string, wantCode int) campaignResponse {
		w := postCampaignWithKey(t, s, body, key)
		if w.Code != wantCode {
			t.Fatalf("key %q status = %d, want %d (body=%s)", key, w.Code, wantCode, w.Body.String())
		}
		var c campaignResponse
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return c
	}

	first := post("the-same-key", http.StatusCreated)
	replay := post("the-same-key", http.StatusOK)
	if first.ID != replay.ID {
		t.Errorf("replay id %s != first id %s; same Idempotency-Key must dedup", replay.ID, first.ID)
	}
	other := post("a-different-key", http.StatusCreated)
	if other.ID == first.ID {
		t.Errorf("different key returned the same id %s; a distinct key must mint a distinct campaign", other.ID)
	}
}

// TestCreateCampaign_RequireWriteScope_403 asserts a bearer token missing
// write:campaigns is rejected 403 insufficient_scope.
func TestCreateCampaign_RequireWriteScope_403(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	id := Identity{Subject: "github:op", TokenID: "tok_no_campaigns", Scopes: []string{"write:runs"}}
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns",
		strings.NewReader(`{"repo":"x/y","epic_ref":"issue:1"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handleCreateCampaign(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", code)
	}
}

// TestCreateCampaign_NilRepo_503 asserts the nil-CampaignRepo guard.
func TestCreateCampaign_NilRepo_503(t *testing.T) {
	s := New(Config{}) // no CampaignRepo
	w := postCampaign(t, s, `{"repo":"x/y","epic_ref":"issue:1"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_repo_unconfigured" {
		t.Errorf("code = %q, want campaign_repo_unconfigured", code)
	}
}

func TestCreateCampaign_BadJSON_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	w := postCampaign(t, s, `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

func TestCreateCampaign_UnknownField_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	w := postCampaign(t, s, `{"repo":"x/y","epic_ref":"issue:1","extra":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on unknown field", w.Code)
	}
}

func TestCreateCampaign_BadRepo_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	w := postCampaign(t, s, `{"repo":"not-a-repo","epic_ref":"issue:1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

func TestCreateCampaign_EmptyEpicRef_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	w := postCampaign(t, s, `{"repo":"x/y","epic_ref":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

// TestCreateCampaign_DanglingDependency_422 asserts a non-empty DroppedEdges
// from the provider fails closed at the Assemble boundary with a typed 422.
func TestCreateCampaign_DanglingDependency_422(t *testing.T) {
	fp := &fakeEpicProvider{result: &workmgmt.EpicChildrenResult{
		Children:     []workmgmt.EpicChild{{Number: 100}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 100, To: 999}},
	}}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_dangling_dependency" {
		t.Errorf("code = %q, want campaign_dangling_dependency", code)
	}
}

// TestCreateCampaign_Cycle_400 asserts a dependency cycle surfaced by
// plan.Waves fails closed with a 400.
func TestCreateCampaign_Cycle_400(t *testing.T) {
	// 100 -> 101 -> 100 is a cycle over the sibling set.
	fp := &fakeEpicProvider{result: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 100}, {Number: 101}},
		Edges:    []workmgmt.DependsEdge{{From: 100, To: 101}, {From: 101, To: 100}},
	}}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

// TestCreateCampaign_EpicChildrenQueryError_502 asserts a provider query
// failure surfaces a 502.
func TestCreateCampaign_EpicChildrenQueryError_502(t *testing.T) {
	fp := &fakeEpicProvider{queryErr: fmt.Errorf("github boom")}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "epic_children_query_failed" {
		t.Errorf("code = %q, want epic_children_query_failed", code)
	}
}

// TestCreateCampaign_ProviderNotQuerier_501 asserts a provider that does NOT
// implement EpicChildrenQuerier fails closed with a typed 501.
func TestCreateCampaign_ProviderNotQuerier_501(t *testing.T) {
	// Register a plain Provider (the work-item fake) that lacks EpicChildren.
	workmgmt.Register(&fakeWorkProvider{name: workmgmt.Default().Provider})
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "epic_children_unsupported" {
		t.Errorf("code = %q, want epic_children_unsupported", code)
	}
}

// TestCreateCampaign_UnknownProvider_501 asserts an unregistered conventions
// provider id fails closed with provider_unimplemented.
func TestCreateCampaign_UnknownProvider_501(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) {
		return workmgmt.Conventions{Provider: "never_registered_provider"}, nil
	}
	t.Cleanup(func() { conventionsLoader = prev })
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "provider_unimplemented" {
		t.Errorf("code = %q, want provider_unimplemented", code)
	}
}

// TestCreateCampaign_ConventionsError_500 asserts a conventions-load failure
// surfaces a 500.
func TestCreateCampaign_ConventionsError_500(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) {
		return workmgmt.Conventions{}, fmt.Errorf("conventions boom")
	}
	t.Cleanup(func() { conventionsLoader = prev })
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
}

// --- next_action precedence (pure function) ---

func TestComputeCampaignNextAction_Precedence(t *testing.T) {
	cases := []struct {
		name               string
		elig               campaign.Eligibility
		want               string
		wantRef            string
		wantDetailContains string
	}{
		{
			// THE #1838 fix: a stuck FAILED item no longer suppresses an ELIGIBLE
			// sibling — the eligible item is dispatched first (start_run) so the
			// campaign keeps making progress while the failed item is quarantined.
			name:    "failed and eligible both present -> start_run (eligible wins)",
			elig:    campaign.Eligibility{Failed: []string{"issue:5"}, Eligible: []string{"issue:6"}},
			want:    "start_run",
			wantRef: "issue:6",
		},
		{
			// PAUSED now outranks a stuck FAILED item (#1838 reorder: a gate
			// hand-off is surfaced before the stuck-failure attention).
			name:    "failed and paused both present -> resume (paused wins)",
			elig:    campaign.Eligibility{Failed: []string{"issue:5"}, Paused: []string{"issue:6"}},
			want:    "resume",
			wantRef: "issue:6",
		},
		{
			// PAUSED outranks ELIGIBLE: a gate hand-off is surfaced before dispatch.
			name:    "paused and eligible both present -> resume",
			elig:    campaign.Eligibility{Paused: []string{"issue:7"}, Eligible: []string{"issue:8"}},
			want:    "resume",
			wantRef: "issue:7",
		},
		{
			name:    "paused only -> resume",
			elig:    campaign.Eligibility{Paused: []string{"issue:9"}},
			want:    "resume",
			wantRef: "issue:9",
		},
		{
			name:    "eligible only -> start_run",
			elig:    campaign.Eligibility{Eligible: []string{"issue:6"}},
			want:    "start_run",
			wantRef: "issue:6",
		},
		{
			// start_run WINS over attend_human_led: an autonomous item is eligible
			// even though a human-led item is ALSO deps-satisfied, so autonomous
			// dispatch is surfaced first and human-led work never stalls it.
			name:    "eligible and human-led both present -> start_run (autonomous wins)",
			elig:    campaign.Eligibility{Eligible: []string{"issue:6"}, HumanLed: []string{"issue:12"}},
			want:    "start_run",
			wantRef: "issue:6",
		},
		{
			// Restartable (#1729): a deps-satisfied cancelled item surfaces as
			// start_run so its dependents unblock — the campaign-wedge fix.
			name:    "restartable only -> start_run",
			elig:    campaign.Eligibility{Restartable: []string{"issue:20"}},
			want:    "start_run",
			wantRef: "issue:20",
		},
		{
			// Eligible OUTRANKS Restartable: a fresh unstarted item is preferred
			// over restarting a cancelled one.
			name:    "eligible and restartable both present -> start_run (eligible wins)",
			elig:    campaign.Eligibility{Eligible: []string{"issue:6"}, Restartable: []string{"issue:20"}},
			want:    "start_run",
			wantRef: "issue:6",
		},
		{
			// Restartable OUTRANKS HumanLed: an autonomous restart is surfaced
			// before human-led work.
			name:    "restartable and human-led both present -> start_run (restartable wins)",
			elig:    campaign.Eligibility{Restartable: []string{"issue:20"}, HumanLed: []string{"issue:12"}},
			want:    "start_run",
			wantRef: "issue:20",
		},
		{
			// A RESTARTABLE item outranks a stuck FAILED item (#1838): the
			// restartable dispatch is surfaced first so a genuinely-stuck failure
			// never suppresses still-actionable work.
			name:    "failed and restartable both present -> start_run (restartable wins)",
			elig:    campaign.Eligibility{Failed: []string{"issue:5"}, Restartable: []string{"issue:20"}},
			want:    "start_run",
			wantRef: "issue:20",
		},
		{
			// attend_human_led fires ONLY when no autonomous item is eligible.
			name:    "human-led only -> attend_human_led",
			elig:    campaign.Eligibility{HumanLed: []string{"issue:12"}},
			want:    "attend_human_led",
			wantRef: "issue:12",
		},
		{
			name: "running only -> wait",
			elig: campaign.Eligibility{Running: []string{"issue:7"}},
			want: "wait",
		},
		{
			name: "blocked only -> wait",
			elig: campaign.Eligibility{Blocked: []string{"issue:8"}},
			want: "wait",
		},
		{
			name: "all done -> complete",
			elig: campaign.Eligibility{Done: []string{"issue:9"}},
			want: "complete",
		},
		{
			// A genuinely-stuck failed item (deps-unsatisfied or human-led, so it
			// stayed in Failed rather than diverting to Restartable) is the ONLY
			// case that still surfaces attention — with the honest #1838 detail
			// that names no verb that refuses (no more "retry or abandon").
			name:               "failed only -> attention (stuck, honest detail)",
			elig:               campaign.Eligibility{Failed: []string{"issue:10"}},
			want:               "attention",
			wantRef:            "issue:10",
			wantDetailContains: "cannot be auto-restarted",
		},
		{
			name: "cancelled only -> complete",
			elig: campaign.Eligibility{Cancelled: []string{"issue:11"}},
			want: "complete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeCampaignNextAction(tc.elig)
			if got.Action != tc.want {
				t.Errorf("action = %q, want %q", got.Action, tc.want)
			}
			if tc.wantRef != "" && got.IssueRef != tc.wantRef {
				t.Errorf("issue_ref = %q, want %q", got.IssueRef, tc.wantRef)
			}
			if tc.wantDetailContains != "" && !strings.Contains(got.Detail, tc.wantDetailContains) {
				t.Errorf("detail = %q, want to contain %q", got.Detail, tc.wantDetailContains)
			}
		})
	}
}

// TestToCampaignRollupPayload_HumanLedNonNil asserts the new human_led slice is
// normalized to a non-nil array (never JSON null) when empty, and passes a
// populated HumanLed partition through.
func TestToCampaignRollupPayload_HumanLedNonNil(t *testing.T) {
	// Empty HumanLed → non-nil array on the wire.
	empty := toCampaignRollupPayload(campaign.Eligibility{})
	if empty.HumanLed == nil {
		t.Errorf("HumanLed = nil, want non-nil empty slice")
	}
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"human_led":[]`) {
		t.Errorf("json = %s, want human_led as []", b)
	}
	// Populated HumanLed flows through unchanged.
	got := toCampaignRollupPayload(campaign.Eligibility{HumanLed: []string{"issue:12"}})
	if !reflect.DeepEqual(got.HumanLed, []string{"issue:12"}) {
		t.Errorf("HumanLed = %v, want [issue:12]", got.HumanLed)
	}
}

// --- read handler tests ---

func TestGetCampaign_BadUUID_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/not-a-uuid", nil)
	req.SetPathValue("campaign_id", "not-a-uuid")
	w := httptest.NewRecorder()
	s.handleGetCampaign(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

func TestGetCampaign_NotFound_404(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+id.String(), nil)
	req.SetPathValue("campaign_id", id.String())
	w := httptest.NewRecorder()
	s.handleGetCampaign(w, withAuth(req))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeCampaignError(t, w); code != "campaign_not_found" {
		t.Errorf("code = %q, want campaign_not_found", code)
	}
}

func TestGetCampaign_NilRepo_503(t *testing.T) {
	s := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+uuid.New().String(), nil)
	req.SetPathValue("campaign_id", uuid.New().String())
	w := httptest.NewRecorder()
	s.handleGetCampaign(w, withAuth(req))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestGetCampaign_HappyPath(t *testing.T) {
	repo := newFakeCampaignRepo()
	c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", nil)
	s := New(Config{CampaignRepo: repo})

	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+c.ID.String(), nil)
	req.SetPathValue("campaign_id", c.ID.String())
	w := httptest.NewRecorder()
	s.handleGetCampaign(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var got campaignResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ID != c.ID || got.EpicRef != "issue:99" {
		t.Errorf("got = %+v, want id/epic_ref echoed", got)
	}
}

func TestListCampaignItems_HappyPath(t *testing.T) {
	repo := newFakeCampaignRepo()
	c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		{IssueRef: "issue:100", State: campaign.ItemStatePending},
		{IssueRef: "issue:101", DependsOn: []string{"issue:100"}, State: campaign.ItemStateBlocked},
	})
	s := New(Config{CampaignRepo: repo})

	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+c.ID.String()+"/items", nil)
	req.SetPathValue("campaign_id", c.ID.String())
	w := httptest.NewRecorder()
	s.handleListCampaignItems(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var body struct {
		Items []campaignItemResponse `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	// depends_on is always a (possibly empty) array, never null.
	if body.Items[0].DependsOn == nil {
		t.Error("item[0].depends_on is nil, want []")
	}
}

func TestListCampaignItems_NotFound_404(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+id.String()+"/items", nil)
	req.SetPathValue("campaign_id", id.String())
	w := httptest.NewRecorder()
	s.handleListCampaignItems(w, withAuth(req))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_not_found" {
		t.Errorf("code = %q, want campaign_not_found", code)
	}
}

func TestListCampaignItems_BadUUID_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/bad/items", nil)
	req.SetPathValue("campaign_id", "bad")
	w := httptest.NewRecorder()
	s.handleListCampaignItems(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestGetCampaign_AccountOwnership pins the GET /v0/campaigns/{id}
// ownership check (ADR-057 / #1830), mirroring enforceAccount for runs:
// a cross-account campaign is 403 account_forbidden, a same-account one is
// allowed, and an untenanted campaign is allowed under any caller (the
// NULL-allow window).
func TestGetCampaign_AccountOwnership(t *testing.T) {
	repo := newFakeCampaignRepo()
	tenanted := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", nil)
	untenanted := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:100", nil)
	acctA, acctB := uuid.NewString(), uuid.NewString()
	repo.accounts[tenanted.ID] = acctA
	s := New(Config{CampaignRepo: repo})

	get := func(campaignID uuid.UUID, callerAcct string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+campaignID.String(), nil)
		req.SetPathValue("campaign_id", campaignID.String())
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity,
			Identity{Subject: "github:op", TokenID: "tok-1", AccountID: callerAcct}))
		w := httptest.NewRecorder()
		s.handleGetCampaign(w, req)
		return w
	}

	// Cross-account: refused 403 account_forbidden.
	if w := get(tenanted.ID, acctB); w.Code != http.StatusForbidden {
		t.Errorf("cross-account status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	} else if code := decodeCampaignError(t, w); code != "account_forbidden" {
		t.Errorf("cross-account code = %q, want account_forbidden", code)
	}

	// Same account: allowed.
	if w := get(tenanted.ID, acctA); w.Code != http.StatusOK {
		t.Errorf("same-account status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// Untenanted campaign: allowed under any (even cross-tenant) caller.
	if w := get(untenanted.ID, acctB); w.Code != http.StatusOK {
		t.Errorf("untenanted status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

// erroringAccountCampaignRepo fails the AccountGetter lookup so the
// degrade-to-untenanted-allow posture is testable.
type erroringAccountCampaignRepo struct{ *fakeCampaignRepo }

func (e erroringAccountCampaignRepo) GetCampaignAccountID(context.Context, uuid.UUID) (string, error) {
	return "", errInjected
}

// noCapabilityCampaignRepo narrows a fake to the plain Repository method
// set (interface embedding drops GetCampaignAccountID), so the
// capability-absent branch is testable.
type noCapabilityCampaignRepo struct{ campaign.Repository }

// TestGetCampaign_AccountOwnership_LookupError_FailsClosed pins the
// security-critical fail-CLOSED posture (ADR-057 / #1830): an AccountGetter
// lookup ERROR must NOT disclose the already-resolved tenanted campaign to a
// cross-account caller. A transient DB failure on the account probe surfaces
// 503 service_unavailable (mirroring bearerAuth's mcp:run account resolution,
// "any lookup ERROR fails CLOSED with 503"), never a fall-open 200.
func TestGetCampaign_AccountOwnership_LookupError_FailsClosed(t *testing.T) {
	inner := newFakeCampaignRepo()
	c := inner.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", nil)
	inner.accounts[c.ID] = uuid.NewString() // tenanted, but the lookup errors before it's read

	s := New(Config{CampaignRepo: erroringAccountCampaignRepo{inner}})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+c.ID.String(), nil)
	req.SetPathValue("campaign_id", c.ID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity,
		Identity{Subject: "github:op", TokenID: "tok-1", AccountID: uuid.NewString()}))
	w := httptest.NewRecorder()
	s.handleGetCampaign(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup_error: status = %d, want 503 (fail closed; body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "service_unavailable" {
		t.Errorf("lookup_error: code = %q, want service_unavailable", code)
	}
}

// TestGetCampaign_AccountOwnership_CapabilityAbsent pins the remaining
// degrade-to-allow branch (ADR-057 / #1830): a repo WITHOUT the optional
// AccountGetter capability degrades to untenanted-allow, matching the
// run.AccountGetter posture bearerAuth uses when the capability is absent.
// This branch is unreachable in production — the concrete postgres repo
// carries AccountGetter (a compile-time assertion) — so only a
// capability-narrowed test fake lands here.
func TestGetCampaign_AccountOwnership_CapabilityAbsent(t *testing.T) {
	inner := newFakeCampaignRepo()
	c := inner.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", nil)
	inner.accounts[c.ID] = uuid.NewString() // tenanted, but the capability-narrowed repo can't see it

	s := New(Config{CampaignRepo: noCapabilityCampaignRepo{inner}})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+c.ID.String(), nil)
	req.SetPathValue("campaign_id", c.ID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity,
		Identity{Subject: "github:op", TokenID: "tok-1", AccountID: uuid.NewString()}))
	w := httptest.NewRecorder()
	s.handleGetCampaign(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("capability_absent: status = %d, want 200 (degrade to allow; body=%s)", w.Code, w.Body.String())
	}
}

func TestGetCampaignStatus_RollupAndNextAction(t *testing.T) {
	repo := newFakeCampaignRepo()
	// A GENUINELY-STUCK failed item (its dependency is unsatisfied, so it stays in
	// the Failed slice rather than diverting to Restartable) alongside an eligible
	// sibling: after #1838 the eligible sibling is dispatched FIRST (start_run) —
	// the stuck failure no longer suppresses actionable work — while the failed
	// item still surfaces in the rollup's Failed partition.
	c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		{IssueRef: "issue:100", State: campaign.ItemStateFailed, DependsOn: []string{"issue:999"}},
		{IssueRef: "issue:101", State: campaign.ItemStatePending},
	})
	s := New(Config{CampaignRepo: repo})

	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+c.ID.String()+"/status", nil)
	req.SetPathValue("campaign_id", c.ID.String())
	w := httptest.NewRecorder()
	s.handleGetCampaignStatus(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var status struct {
		Rollup     campaignRollupPayload     `json:"rollup"`
		NextAction campaignNextActionPayload `json:"next_action"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	if len(status.Rollup.Failed) != 1 || status.Rollup.Failed[0] != "issue:100" {
		t.Errorf("rollup.Failed = %v, want [issue:100]", status.Rollup.Failed)
	}
	// The eligible sibling is dispatched first; the stuck failure is quarantined.
	if status.NextAction.Action != "start_run" || status.NextAction.IssueRef != "issue:101" {
		t.Errorf("next_action = %+v, want start_run issue:101", status.NextAction)
	}
}

func TestGetCampaignStatus_NotFound_404(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+id.String()+"/status", nil)
	req.SetPathValue("campaign_id", id.String())
	w := httptest.NewRecorder()
	s.handleGetCampaignStatus(w, withAuth(req))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeCampaignError(t, w); code != "campaign_not_found" {
		t.Errorf("code = %q, want campaign_not_found", code)
	}
}

func TestGetCampaignStatus_NilRepo_503(t *testing.T) {
	s := New(Config{})
	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+id.String()+"/status", nil)
	req.SetPathValue("campaign_id", id.String())
	w := httptest.NewRecorder()
	s.handleGetCampaignStatus(w, withAuth(req))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// --- list handler tests ---

func TestListCampaigns_PaginationAndEmpty(t *testing.T) {
	repo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: repo})

	// Empty list first.
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns", nil)
	w := httptest.NewRecorder()
	s.handleListCampaigns(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var empty struct {
		Items      []campaignResponse `json:"items"`
		NextCursor string             `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &empty)
	if len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Errorf("empty list = %+v, want no items and no cursor", empty)
	}

	// Seed 3 campaigns; page size 2 -> a next_cursor, then the tail.
	for i := 0; i < 3; i++ {
		repo.seedCampaignWithItems("kuhlman-labs/fishhawk", fmt.Sprintf("issue:%d", i), nil)
	}
	req = httptest.NewRequest(http.MethodGet, "/v0/campaigns?limit=2", nil)
	w = httptest.NewRecorder()
	s.handleListCampaigns(w, withAuth(req))
	var page1 struct {
		Items      []campaignResponse `json:"items"`
		NextCursor string             `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1.Items) != 2 {
		t.Fatalf("page1 items = %d, want 2", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 next_cursor empty, want a cursor")
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/campaigns?limit=2&cursor="+page1.NextCursor, nil)
	w = httptest.NewRecorder()
	s.handleListCampaigns(w, withAuth(req))
	var page2 struct {
		Items      []campaignResponse `json:"items"`
		NextCursor string             `json:"next_cursor"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page2)
	if len(page2.Items) != 1 {
		t.Fatalf("page2 items = %d, want 1 (the tail)", len(page2.Items))
	}
	if page2.NextCursor != "" {
		t.Errorf("page2 next_cursor = %q, want empty (end of list)", page2.NextCursor)
	}
}

// TestListCampaigns_AccountScope_PassesThrough pins the account-scope
// wire-up (ADR-057 / #1830): the handler hands the caller's
// Identity.AccountID to the repo via ListCampaignsFilter, and an
// untenanted caller passes "" (no constraint — the unchanged view).
func TestListCampaigns_AccountScope_PassesThrough(t *testing.T) {
	repo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: repo})

	acct := uuid.NewString()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity,
		Identity{Subject: "github:op", TokenID: "tok-1", AccountID: acct}))
	w := httptest.NewRecorder()
	s.handleListCampaigns(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if repo.lastListFilter.AccountID != acct {
		t.Errorf("filter AccountID = %q, want %q", repo.lastListFilter.AccountID, acct)
	}

	// Untenanted caller (no AccountID on the identity): empty filter —
	// unnarrowed, per ListCampaignsFilter.AccountID's contract.
	req = httptest.NewRequest(http.MethodGet, "/v0/campaigns", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity,
		Identity{Subject: "github:op", TokenID: "tok-1"}))
	w = httptest.NewRecorder()
	s.handleListCampaigns(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if repo.lastListFilter.AccountID != "" {
		t.Errorf("untenanted filter AccountID = %q, want empty", repo.lastListFilter.AccountID)
	}
}

func TestListCampaigns_BadState_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns?state=bogus", nil)
	w := httptest.NewRecorder()
	s.handleListCampaigns(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

func TestListCampaigns_BadCursor_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns?cursor=!!!notbase64", nil)
	w := httptest.NewRecorder()
	s.handleListCampaigns(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if code := decodeCampaignError(t, w); code != "cursor_invalid" {
		t.Errorf("code = %q, want cursor_invalid", code)
	}
}

func TestListCampaigns_NilRepo_503(t *testing.T) {
	s := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns", nil)
	w := httptest.NewRecorder()
	s.handleListCampaigns(w, withAuth(req))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// --- resume handler tests ---

// postResume POSTs to handleResumeCampaign with an operator identity (scope
// bypass via withAuth) for the given campaign id.
func postResume(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+id+"/resume", nil)
	req.SetPathValue("campaign_id", id)
	w := httptest.NewRecorder()
	s.handleResumeCampaign(w, withAuth(req))
	return w
}

// seedPausedCampaign seeds a campaign in StatePaused with one paused item
// carrying a PauseReason (the pause_campaign policy shape), plus a succeeded
// sibling so a later resume has work to continue.
func seedPausedCampaign(f *fakeCampaignRepo) (*campaign.Campaign, *campaign.Item) {
	paused := &campaign.Item{
		IssueRef:    "issue:200",
		State:       campaign.ItemStatePaused,
		PauseReason: &campaign.PauseReason{PageEvent: "campaign_gate_paged"},
	}
	c := f.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		{IssueRef: "issue:201", State: campaign.ItemStateSucceeded},
		paused,
	})
	c.State = campaign.StatePaused
	c.PausePolicy = campaign.PausePolicyPauseCampaign
	return c, paused
}

// TestResumeCampaign_HappyPath asserts a paused campaign + paused item flip
// back to running on resume (the pause_campaign policy shape).
func TestResumeCampaign_HappyPath(t *testing.T) {
	repo := newFakeCampaignRepo()
	c, paused := seedPausedCampaign(repo)
	s := New(Config{CampaignRepo: repo})

	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var got campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != string(campaign.StateRunning) {
		t.Errorf("campaign state = %q, want running", got.State)
	}
	// The paused item is now running again.
	resumed := getItemByID(t, repo, c.ID, paused.ID)
	if resumed.State != campaign.ItemStateRunning {
		t.Errorf("item state = %q, want running", resumed.State)
	}
}

// TestResumeCampaign_PauseItemPolicy_ResumesItemOnly asserts the pause_item
// shape: the campaign was never paused (only the item), so resume leaves the
// campaign running and flips just the paused item — and is NOT a 409.
func TestResumeCampaign_PauseItemPolicy_ResumesItemOnly(t *testing.T) {
	repo := newFakeCampaignRepo()
	paused := &campaign.Item{IssueRef: "issue:300", State: campaign.ItemStatePaused}
	c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		{IssueRef: "issue:301", State: campaign.ItemStateRunning},
		paused,
	})
	// pause_item leaves the campaign running (seedCampaignWithItems default).
	c.PausePolicy = campaign.PausePolicyPauseItem
	s := New(Config{CampaignRepo: repo})

	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var got campaignResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.State != string(campaign.StateRunning) {
		t.Errorf("campaign state = %q, want running (was never paused)", got.State)
	}
	if resumed := getItemByID(t, repo, c.ID, paused.ID); resumed.State != campaign.ItemStateRunning {
		t.Errorf("item state = %q, want running", resumed.State)
	}
}

// TestResumeCampaign_NotPaused_409 asserts a running campaign with no paused
// item has nothing to resume -> 409 campaign_not_paused.
func TestResumeCampaign_NotPaused_409(t *testing.T) {
	repo := newFakeCampaignRepo()
	c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		{IssueRef: "issue:400", State: campaign.ItemStateRunning},
	})
	s := New(Config{CampaignRepo: repo})

	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_not_paused" {
		t.Errorf("code = %q, want campaign_not_paused", code)
	}
}

// TestResumeCampaign_NotFound_404 asserts an unknown campaign id -> 404.
func TestResumeCampaign_NotFound_404(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	w := postResume(t, s, uuid.New().String())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_not_found" {
		t.Errorf("code = %q, want campaign_not_found", code)
	}
}

// TestResumeCampaign_BadUUID_400 asserts a malformed campaign id -> 400.
func TestResumeCampaign_BadUUID_400(t *testing.T) {
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})
	w := postResume(t, s, "not-a-uuid")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

// TestResumeCampaign_RequireWriteScope_403 asserts a bearer token missing
// write:campaigns is rejected 403.
func TestResumeCampaign_RequireWriteScope_403(t *testing.T) {
	repo := newFakeCampaignRepo()
	c, _ := seedPausedCampaign(repo)
	s := New(Config{CampaignRepo: repo})
	id := Identity{Subject: "github:op", TokenID: "tok_no_campaigns", Scopes: []string{"write:runs"}}
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+c.ID.String()+"/resume", nil)
	req.SetPathValue("campaign_id", c.ID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handleResumeCampaign(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", code)
	}
}

// TestResumeCampaign_NilRepo_503 asserts the nil-CampaignRepo guard fires
// BEFORE the write-scope check (so an unconfigured deploy answers 503).
func TestResumeCampaign_NilRepo_503(t *testing.T) {
	s := New(Config{})
	w := postResume(t, s, uuid.New().String())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_repo_unconfigured" {
		t.Errorf("code = %q, want campaign_repo_unconfigured", code)
	}
}

// TestResumeCampaign_ItemTransitionError_500 asserts an item-transition
// failure surfaces a 500 (the defensive item-resume branch).
func TestResumeCampaign_ItemTransitionError_500(t *testing.T) {
	repo := newFakeCampaignRepo()
	c, _ := seedPausedCampaign(repo)
	repo.transItemErr = fmt.Errorf("boom")
	s := New(Config{CampaignRepo: repo})

	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
}

func getItemByID(t *testing.T, repo *fakeCampaignRepo, campaignID, itemID uuid.UUID) *campaign.Item {
	t.Helper()
	items, err := repo.ListCampaignItemsForCampaign(context.Background(), campaignID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.ID == itemID {
			return it
		}
	}
	t.Fatalf("item %s not found", itemID)
	return nil
}

// TestGetCampaignStatus_PausedRollupAndNextAction asserts a paused item lands
// in rollup.paused and drives the resume next_action (E25.7).
func TestGetCampaignStatus_PausedRollupAndNextAction(t *testing.T) {
	repo := newFakeCampaignRepo()
	c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		{IssueRef: "issue:500", State: campaign.ItemStatePaused},
		{IssueRef: "issue:501", State: campaign.ItemStateSucceeded},
	})
	s := New(Config{CampaignRepo: repo})

	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+c.ID.String()+"/status", nil)
	req.SetPathValue("campaign_id", c.ID.String())
	w := httptest.NewRecorder()
	s.handleGetCampaignStatus(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var status struct {
		Rollup     campaignRollupPayload     `json:"rollup"`
		NextAction campaignNextActionPayload `json:"next_action"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	if len(status.Rollup.Paused) != 1 || status.Rollup.Paused[0] != "issue:500" {
		t.Errorf("rollup.Paused = %v, want [issue:500]", status.Rollup.Paused)
	}
	if status.NextAction.Action != "resume" || status.NextAction.IssueRef != "issue:500" {
		t.Errorf("next_action = %+v, want resume issue:500", status.NextAction)
	}
}

// TestCreateCampaign_PausePolicyRoundTrip is the binding-condition-2 guard:
// pause_policy survives request -> assembly -> persist -> response across the
// real Postgres boundary. POST with pause_policy=pause_item, then GET and
// assert it round-trips as pause_item (not silently defaulted to
// pause_campaign).
func TestCreateCampaign_PausePolicyRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := campaign.NewPostgresRepository(pool)

	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	gh := recordingInstallGitHubClient(t, 9911, &installRecorder{})
	s := New(Config{CampaignRepo: repo, GitHub: gh})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","pause_policy":"pause_item"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	// The create response already carries the persisted policy.
	if created.PausePolicy != string(campaign.PausePolicyPauseItem) {
		t.Errorf("create response pause_policy = %q, want pause_item", created.PausePolicy)
	}

	// And it round-trips through a fresh GET (read from Postgres, not the
	// in-memory create result).
	getReq := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+created.ID.String(), nil)
	getReq.SetPathValue("campaign_id", created.ID.String())
	gw := httptest.NewRecorder()
	s.handleGetCampaign(gw, withAuth(getReq))
	if gw.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body=%s)", gw.Code, gw.Body.String())
	}
	var got campaignResponse
	_ = json.Unmarshal(gw.Body.Bytes(), &got)
	if got.PausePolicy != string(campaign.PausePolicyPauseItem) {
		t.Errorf("GET pause_policy = %q, want pause_item (round-trip), got silently defaulted", got.PausePolicy)
	}
}

// TestCreateCampaign_BadPausePolicy_400 asserts an unrecognized pause_policy
// fails closed at the handler with a 400 rather than reaching the column CHECK.
func TestCreateCampaign_BadPausePolicy_400(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()})

	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99","pause_policy":"pause_everything"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
}

// pagingGateActor is a campaigndriver.GateActor that forces the Paged outcome
// on the first running run it sees (a deterministic stand-in for a
// must_page_human reviewer_reject gate; the real autodrive reviewer_reject ->
// Paged path is covered by autodrive_test.go and driver_test.go). It pages
// exactly once so a re-tick after resume re-engages mechanically.
type pagingGateActor struct{ paged bool }

func (a *pagingGateActor) DriveRunGate(_ context.Context, _ *run.Run) (campaigndriver.GateActionOutcome, error) {
	if a.paged {
		return campaigndriver.GateActionOutcome{Note: "already paged"}, nil
	}
	a.paged = true
	return campaigndriver.GateActionOutcome{Paged: true, PageEvent: "campaign_gate_paged", Note: "must page human"}, nil
}

// recordingPageNotifier is a campaigndriver.Notifier that records the run ids
// it was asked to page, so the e2e can assert the page fired.
type recordingPageNotifier struct{ runs []uuid.UUID }

func (n *recordingPageNotifier) NotifyStatusUpdateForRun(_ context.Context, runID uuid.UUID) error {
	n.runs = append(n.runs, runID)
	return nil
}

// e2eRunStarter mints real run rows so the driver's terminal detection reads
// genuine persisted state, recording the run id per issue ref.
type e2eRunStarter struct {
	runs  run.Repository
	byRef map[string]uuid.UUID
}

func (s *e2eRunStarter) StartCampaignRun(ctx context.Context, item *campaign.Item, c *campaign.Campaign) (*run.Run, error) {
	ref := item.IssueRef
	r, err := s.runs.CreateRun(ctx, run.CreateRunParams{
		Repo:          c.Repo,
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    &ref,
	})
	if err != nil {
		return nil, err
	}
	s.byRef[ref] = r.ID
	return r, nil
}

// TestResumeCampaign_CrossBoundary_E2E is the done-means of #1446: over a real
// Postgres, the auto-driver pages a human at a run gate (pause_campaign
// policy), pausing the affected item AND the campaign and firing the page;
// the operator then POSTs /resume; and the next driver tick re-engages the
// campaign and the continuation proceeds. This crosses driver -> persistence
// -> notifier -> REST resume -> driver, where a per-layer unit would pass while
// the seam breaks (#618).
func TestResumeCampaign_CrossBoundary_E2E(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	campaigns := campaign.NewPostgresRepository(pool)
	runs := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	// A single-issue campaign in state running with the item already running on
	// a non-terminal run — the gate the auto-driver will page.
	c, err := campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo: "kuhlman-labs/fishhawk", EpicRef: "issue:1000",
		PausePolicy: campaign.PausePolicyPauseCampaign,
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	item, err := campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{
		CampaignID: c.ID, IssueRef: "issue:1",
	})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if _, err := campaigns.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("transition campaign to running: %v", err)
	}
	// Mint the item's run and link it; drive it to running (non-terminal) so the
	// driver hands it to the GateActor rather than settling it.
	starter := &e2eRunStarter{runs: runs, byRef: map[string]uuid.UUID{}}
	runRow, err := starter.StartCampaignRun(ctx, item, c)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if _, err := campaigns.SetCampaignItemRun(ctx, item.ID, &runRow.ID); err != nil {
		t.Fatalf("link item run: %v", err)
	}
	if _, err := campaigns.TransitionCampaignItem(ctx, item.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("transition item to running: %v", err)
	}
	if _, err := runs.TransitionRun(ctx, runRow.ID, run.StateRunning); err != nil {
		t.Fatalf("transition run to running: %v", err)
	}

	notifier := &recordingPageNotifier{}
	tk := &campaigndriver.Ticker{
		Campaigns:   campaigns,
		Runs:        runs,
		Starter:     starter,
		Audit:       auditRepo,
		GateActor:   &pagingGateActor{},
		Notifier:    notifier,
		MaxParallel: 4,
	}

	// --- tick 1: the gate is paged -> item paused, campaign paused, page fired ---
	tk.Tick(ctx)

	pausedItem := e2eGetItem(t, campaigns, c.ID, item.ID)
	if pausedItem.State != campaign.ItemStatePaused {
		t.Fatalf("after page: item = %s, want paused", pausedItem.State)
	}
	if pausedItem.PauseReason == nil || pausedItem.PauseReason.PageEvent != "campaign_gate_paged" {
		t.Errorf("after page: pause_reason = %+v, want campaign_gate_paged", pausedItem.PauseReason)
	}
	gotC, err := campaigns.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign: %v", err)
	}
	if gotC.State != campaign.StatePaused {
		t.Fatalf("after page: campaign = %s, want paused (pause_campaign policy)", gotC.State)
	}
	if len(notifier.runs) != 1 || notifier.runs[0] != runRow.ID {
		t.Errorf("page notifier runs = %v, want [%s]", notifier.runs, runRow.ID)
	}
	assertCampaignPausedAudit(t, auditRepo, c.ID)

	// --- operator resumes via the REST handler ---
	s := New(Config{CampaignRepo: campaigns})
	rw := postResume(t, s, c.ID.String())
	if rw.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200 (body=%s)", rw.Code, rw.Body.String())
	}
	resumedC, err := campaigns.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign after resume: %v", err)
	}
	if resumedC.State != campaign.StateRunning {
		t.Fatalf("after resume: campaign = %s, want running", resumedC.State)
	}
	if it := e2eGetItem(t, campaigns, c.ID, item.ID); it.State != campaign.ItemStateRunning {
		t.Fatalf("after resume: item = %s, want running", it.State)
	}

	// --- tick 2: the run reaches terminal-succeeded; the driver re-engages and
	// the campaign continues to succeeded (the resumed item settles) ---
	if _, err := runs.TransitionRun(ctx, runRow.ID, run.StateSucceeded); err != nil {
		t.Fatalf("transition run to succeeded: %v", err)
	}
	tk.Tick(ctx)

	finalC, err := campaigns.GetCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("get campaign final: %v", err)
	}
	if finalC.State != campaign.StateSucceeded {
		t.Fatalf("after resume+tick: campaign = %s, want succeeded (continued)", finalC.State)
	}
}

func e2eGetItem(t *testing.T, repo campaign.Repository, campaignID, itemID uuid.UUID) *campaign.Item {
	t.Helper()
	items, err := repo.ListCampaignItemsForCampaign(context.Background(), campaignID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, it := range items {
		if it.ID == itemID {
			return it
		}
	}
	t.Fatalf("item %s not found", itemID)
	return nil
}

func assertCampaignPausedAudit(t *testing.T, au audit.Repository, campaignID uuid.UUID) {
	t.Helper()
	entries, err := au.ListGlobal(context.Background())
	if err != nil {
		t.Fatalf("list global audit: %v", err)
	}
	for _, e := range entries {
		if e.Category == "campaign_paused" && strings.Contains(string(e.Payload), campaignID.String()) {
			return
		}
	}
	t.Fatalf("no campaign_paused audit entry found for campaign %s", campaignID)
}

// TestResumeCampaign_GetError_500 asserts a GetCampaign error (not
// ErrNotFound) surfaces a 500 — the campaign-load defensive branch.
func TestResumeCampaign_GetError_500(t *testing.T) {
	repo := newFakeCampaignRepo()
	repo.getErr = fmt.Errorf("db down")
	s := New(Config{CampaignRepo: repo})
	w := postResume(t, s, uuid.New().String())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
}

// TestResumeCampaign_ListItemsError_500 asserts a ListCampaignItemsForCampaign
// error surfaces a 500 — the items-load defensive branch.
func TestResumeCampaign_ListItemsError_500(t *testing.T) {
	repo := newFakeCampaignRepo()
	c, _ := seedPausedCampaign(repo)
	repo.itemsErr = fmt.Errorf("db down")
	s := New(Config{CampaignRepo: repo})
	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
}

// TestResumeCampaign_CampaignTransitionInvalid_409 asserts an
// InvalidTransitionError from the campaign transition surfaces 409
// invalid_transition (the concurrent-change defensive branch).
func TestResumeCampaign_CampaignTransitionInvalid_409(t *testing.T) {
	repo := newFakeCampaignRepo()
	c, _ := seedPausedCampaign(repo)
	repo.transCmpErr = campaign.InvalidTransitionError{Kind: "campaign", From: "paused", To: "running"}
	s := New(Config{CampaignRepo: repo})
	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "invalid_transition" {
		t.Errorf("code = %q, want invalid_transition", code)
	}
}

// TestResumeCampaign_CampaignTransitionError_500 asserts a non-transition
// error from the campaign transition surfaces a 500.
func TestResumeCampaign_CampaignTransitionError_500(t *testing.T) {
	repo := newFakeCampaignRepo()
	c, _ := seedPausedCampaign(repo)
	repo.transCmpErr = fmt.Errorf("db down")
	s := New(Config{CampaignRepo: repo})
	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
}

// TestResumeCampaign_ItemTransitionInvalid_409 asserts an
// InvalidTransitionError from the item transition surfaces 409
// invalid_transition.
func TestResumeCampaign_ItemTransitionInvalid_409(t *testing.T) {
	repo := newFakeCampaignRepo()
	c, _ := seedPausedCampaign(repo)
	repo.transItemErr = campaign.InvalidTransitionError{Kind: "campaign_item", From: "paused", To: "running"}
	s := New(Config{CampaignRepo: repo})
	w := postResume(t, s, c.ID.String())
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "invalid_transition" {
		t.Errorf("code = %q, want invalid_transition", code)
	}
}

// --- start campaign item run (E26.2 / #1481) ---

// campaignAuditRecorder records AppendGlobalChained calls so the start +
// reconcile tests can assert the campaign_issue_started / _settled / _advanced
// markers land (and, for idempotency, that a re-poll emits none).
type campaignAuditRecorder struct {
	audit.BaseFake
	mu      sync.Mutex
	entries []audit.GlobalChainAppendParams
}

func (a *campaignAuditRecorder) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, p)
	return &audit.Entry{ID: uuid.New(), Category: p.Category, Payload: p.Payload}, nil
}

func (a *campaignAuditRecorder) count(category string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.entries {
		if e.Category == category {
			n++
		}
	}
	return n
}

// cItem builds a campaign item fixture in the given state with the given deps.
func cItem(ref string, deps []string, state campaign.ItemState) *campaign.Item {
	return &campaign.Item{IssueRef: ref, DependsOn: deps, State: state}
}

// newCampaignStartServer wires a Server with the fake campaign repo plus a run
// repo, a recording audit repo, and a GitHub stub serving the installation +
// gatedSpecYAML so StartRunForCampaignIssue → CreateRunForTrigger actually mints
// a run end-to-end.
func newCampaignStartServer(t *testing.T, crepo *fakeCampaignRepo) (*Server, *fakeRepo, *campaignAuditRecorder) {
	t.Helper()
	rrepo := newFakeRepo()
	aud := &campaignAuditRecorder{}
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	ghSrv := fake.server(t)
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
	s := New(Config{Addr: "127.0.0.1:0", CampaignRepo: crepo, RunRepo: rrepo, AuditRepo: aud, GitHub: gh})
	return s, rrepo, aud
}

// postStartItemRun POSTs a start body to handleStartCampaignItemRun with an
// operator identity (scope bypass).
func postStartItemRun(t *testing.T, s *Server, campaignID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+campaignID.String()+"/runs", strings.NewReader(body))
	req.SetPathValue("campaign_id", campaignID.String())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleStartCampaignItemRun(w, withAuth(req))
	return w
}

type startItemRunBody struct {
	Run  runResponse          `json:"run"`
	Item campaignItemResponse `json:"item"`
}

type campaignStatusBody struct {
	Campaign   campaignResponse          `json:"campaign"`
	Items      []campaignItemResponse    `json:"items"`
	Rollup     campaignRollupPayload     `json:"rollup"`
	NextAction campaignNextActionPayload `json:"next_action"`
}

// getCampaignStatusBody GETs /status with an operator identity and decodes the
// rollup body.
func getCampaignStatusBody(t *testing.T, s *Server, campaignID uuid.UUID) campaignStatusBody {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+campaignID.String()+"/status", nil)
	req.SetPathValue("campaign_id", campaignID.String())
	w := httptest.NewRecorder()
	s.handleGetCampaignStatus(w, withAuth(req))
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var st campaignStatusBody
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, w.Body.String())
	}
	return st
}

// TestStartCampaignItemRun_Eligible_CrossBoundary_E2E is the headline
// cross-layer done-means: a request for an eligible item crosses HTTP → DAG gate
// → StartRunForCampaignIssue → CreateRunForTrigger → run, links the item, moves
// it to running, and advances a pending campaign to running — asserting the run
// carries runner_kind=local so the local loop drives it.
func TestStartCampaignItemRun_Eligible_CrossBoundary_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, aud := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
		cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Run.RunnerKind != "local" {
		t.Errorf("run runner_kind = %q, want local", body.Run.RunnerKind)
	}
	if body.Run.TriggerSource != "github_issue" || body.Run.TriggerRef == nil || *body.Run.TriggerRef != "issue:100" {
		t.Errorf("run trigger = %s/%v, want github_issue/issue:100", body.Run.TriggerSource, body.Run.TriggerRef)
	}
	if body.Item.State != string(campaign.ItemStateRunning) {
		t.Errorf("item state = %q, want running", body.Item.State)
	}
	if body.Item.RunID == nil || *body.Item.RunID != body.Run.ID {
		t.Errorf("item run_id = %v, want linked to %s", body.Item.RunID, body.Run.ID)
	}
	// The pending campaign advanced to running on its first dispatch.
	if got := crepo.campaigns[c.ID].State; got != campaign.StateRunning {
		t.Errorf("campaign state = %q, want running after first dispatch", got)
	}
	if n := aud.count("campaign_issue_started"); n != 1 {
		t.Errorf("campaign_issue_started count = %d, want 1", n)
	}
	if n := aud.count("campaign_advanced"); n != 1 {
		t.Errorf("campaign_advanced count = %d, want 1 (pending→running)", n)
	}
}

// TestStartCampaignItemRun_ReconcileOnRead_SettlesAndAdvances_E2E starts an
// eligible item, flips its linked run to terminal succeeded, and asserts the
// status read settles the item done, advances next_action to the now-eligible
// dependent, and that a second poll performs NO further transition/audit
// (idempotency / no double-transition).
func TestStartCampaignItemRun_ReconcileOnRead_SettlesAndAdvances_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
		cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)

	// Flip the linked run to terminal succeeded, then read status: reconcile
	// settles the item and the dependent becomes eligible.
	if _, err := rrepo.TransitionRun(context.Background(), body.Run.ID, run.StateSucceeded); err != nil {
		t.Fatalf("flip run terminal: %v", err)
	}
	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 1 || st.Rollup.Done[0] != "issue:100" {
		t.Errorf("rollup.Done = %v, want [issue:100] after settle", st.Rollup.Done)
	}
	if len(st.Rollup.Eligible) != 1 || st.Rollup.Eligible[0] != "issue:101" {
		t.Errorf("rollup.Eligible = %v, want [issue:101] after predecessor settled", st.Rollup.Eligible)
	}
	if st.NextAction.Action != "start_run" || st.NextAction.IssueRef != "issue:101" {
		t.Errorf("next_action = %+v, want start_run issue:101", st.NextAction)
	}
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Errorf("campaign_issue_settled count = %d, want 1", n)
	}

	// Idempotent re-poll: no further transition or settle audit.
	settledBefore := aud.count("campaign_issue_settled")
	transBefore := crepo.transItemCalls
	_ = getCampaignStatusBody(t, s, c.ID)
	if got := aud.count("campaign_issue_settled"); got != settledBefore {
		t.Errorf("campaign_issue_settled grew on re-poll: %d → %d (double-transition)", settledBefore, got)
	}
	if got := crepo.transItemCalls; got != transBefore {
		t.Errorf("TransitionCampaignItem calls grew on re-poll: %d → %d", transBefore, got)
	}
}

// TestStartCampaignItemRun_ReconcileOnRead_FailedRun_TerminalFailed_E2E asserts
// the #1838 anti-over-fix guard at the reconcile seam: a SINGLE-item campaign
// whose only linked run fails settles the item failed and the campaign derives
// to FAILED (all items terminal, at least one failed — genuinely terminal). The
// dispatched-then-failed item is deps-satisfied and non-human-led, so NextEligible
// diverts it to Restartable (folded into the wire cancelled slice), and
// next_action surfaces start_run. NOTE the residual: the campaign is terminal, so
// handleStartCampaignItemRun would refuse the restart (campaign_not_startable) —
// flagged as a known single-item/all-terminal limitation (see PR Notes). The
// multi-item quarantine done-means is covered by the _Quarantine_ e2e below.
func TestStartCampaignItemRun_ReconcileOnRead_FailedRun_TerminalFailed_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, err := rrepo.TransitionRun(context.Background(), body.Run.ID, run.StateFailed); err != nil {
		t.Fatalf("flip run failed: %v", err)
	}
	st := getCampaignStatusBody(t, s, c.ID)
	// The single all-terminal failed item drives the campaign genuinely failed
	// (the anti-over-fix guard — no actionable sibling remains).
	if st.Campaign.State != string(campaign.StateFailed) {
		t.Errorf("campaign state = %q, want failed (single all-terminal failed item)", st.Campaign.State)
	}
	// The failed item is restartable (deps-satisfied, non-human-led), so it is NOT
	// in the Failed rollup slice — it is folded into the wire cancelled slice.
	if len(st.Rollup.Failed) != 0 {
		t.Errorf("rollup.Failed = %v, want [] (restartable item folded into cancelled)", st.Rollup.Failed)
	}
	if len(st.Rollup.Cancelled) != 1 || st.Rollup.Cancelled[0] != "issue:100" {
		t.Errorf("rollup.Cancelled = %v, want [issue:100] (restartable folded)", st.Rollup.Cancelled)
	}
	if st.NextAction.Action != "start_run" || st.NextAction.IssueRef != "issue:100" {
		t.Errorf("next_action = %+v, want start_run issue:100 (restartable)", st.NextAction)
	}
	if n := aud.count("campaign_advanced"); n < 1 {
		t.Errorf("campaign_advanced count = %d, want >=1 (running→failed)", n)
	}
}

// TestStartCampaignItemRun_ReconcileOnRead_FailedRun_Quarantine_E2E is the #1838
// PRIMARY done-means at the engine->reconcile-on-read->status seam: a multi-item
// campaign where one linked run FAILS while an independent sibling is still
// eligible reconciles to a RUNNING campaign (the failed item is quarantined, not
// campaign-terminal) with next_action start_run on the still-actionable sibling —
// so the failed item no longer drives the whole campaign terminal.
func TestStartCampaignItemRun_ReconcileOnRead_FailedRun_Quarantine_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	// issue:100 is dispatched and will fail; issue:101 is an independent pending
	// item (no deps) that stays eligible throughout.
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
		cItem("issue:101", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, err := rrepo.TransitionRun(context.Background(), body.Run.ID, run.StateFailed); err != nil {
		t.Fatalf("flip run failed: %v", err)
	}
	st := getCampaignStatusBody(t, s, c.ID)
	// Quarantine: the failed item alongside an eligible sibling keeps the campaign
	// RUNNING (NOT failed) so the sibling can still be driven.
	if st.Campaign.State != string(campaign.StateRunning) {
		t.Errorf("campaign state = %q, want running (failed item quarantined, sibling still eligible)", st.Campaign.State)
	}
	if len(st.Rollup.Eligible) != 1 || st.Rollup.Eligible[0] != "issue:101" {
		t.Errorf("rollup.Eligible = %v, want [issue:101]", st.Rollup.Eligible)
	}
	// next_action dispatches the still-eligible sibling — a restartable failed item
	// (issue:100) is outranked by the fresh eligible one (issue:101).
	if st.NextAction.Action != "start_run" || st.NextAction.IssueRef != "issue:101" {
		t.Errorf("next_action = %+v, want start_run issue:101 (eligible sibling)", st.NextAction)
	}
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Errorf("campaign_issue_settled count = %d, want 1", n)
	}
}

// TestDeriveCampaignAfterChange_CampaignStart_SweepsToUpNext_CrossBoundary is
// the #1816 cross-boundary done-means: driving a pending campaign to running
// through deriveCampaignAfterChange sweeps its still-QUEUED items onto the
// board's Up Next column via the campaign_started edge. It crosses the campaign
// driver -> board-sync hook -> registered Transitioner -> global-chain audit
// seam and asserts (1) each still-pending item fired campaign_started -> Up Next
// with a work_item_transitioned audit on the GLOBAL chain, (2) the already-
// running item was NOT campaign_started, and (3) run_started later advances an
// Up Next card to In Progress (up_next is in run_started's expected source).
func TestDeriveCampaignAfterChange_CampaignStart_SweepsToUpNext_CrossBoundary(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{Moved: true, From: "Backlog", To: "Up Next"}}
	registerTransitionProvider(t, fp)

	crepo := newFakeCampaignRepo()
	au := &campaignAuditRecorder{}
	gh := recordingInstallGitHubClient(t, 12345, &installRecorder{})
	s := New(Config{CampaignRepo: crepo, AuditRepo: au, GitHub: gh})

	// A PENDING campaign: one item already running (the just-dispatched one that
	// drives the pending->running derivation), two still-queued pending items.
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStateRunning),
		cItem("issue:101", nil, campaign.ItemStatePending),
		cItem("issue:102", []string{"issue:101"}, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending

	items, err := crepo.ListCampaignItemsForCampaign(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	s.deriveCampaignAfterChange(context.Background(), c, items)

	// The campaign advanced pending -> running.
	if got := crepo.campaigns[c.ID].State; got != campaign.StateRunning {
		t.Fatalf("campaign state = %q, want running", got)
	}

	// (1)+(2): both still-pending items (101, 102) fired campaign_started -> Up
	// Next; the already-running item (100) was excluded.
	if len(fp.calls) != 2 {
		t.Fatalf("Transition calls = %d, want 2 (only the two pending items)", len(fp.calls))
	}
	fired := map[int]bool{}
	for _, call := range fp.calls {
		if call.Trigger != lifecycleCampaignStarted {
			t.Errorf("trigger = %q, want campaign_started", call.Trigger)
		}
		if call.CanonicalState != workmgmt.CanonicalStateUpNext {
			t.Errorf("canonical = %q, want up_next", call.CanonicalState)
		}
		if call.Target.Scope != forge.FromGitHubInstallationID(12345) {
			t.Errorf("scope = %q, want scope for installation 12345 (resolved from repo)", call.Target.Scope.Ref())
		}
		fired[call.IssueNumber] = true
	}
	if !fired[101] || !fired[102] {
		t.Errorf("fired issues = %v, want 101 and 102", fired)
	}
	if fired[100] {
		t.Errorf("running item issue:100 fired campaign_started, want excluded")
	}
	if got := au.count(categoryWorkItemTransitioned); got != 2 {
		t.Errorf("work_item_transitioned audits = %d, want 2", got)
	}

	// (3) run_started advances an Up Next card: firing the run_started edge for a
	// now-Up-Next card dispatches In Progress with up_next in the expected source.
	fp.calls = nil
	inst := int64(99)
	ref := "issue:101"
	rn := &run.Run{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk", State: run.StateRunning, TriggerRef: &ref, InstallationID: &inst}
	s.boardTransitionForRun(context.Background(), rn, lifecycleRunStarted)
	if len(fp.calls) != 1 {
		t.Fatalf("run_started Transition calls = %d, want 1", len(fp.calls))
	}
	if fp.calls[0].CanonicalState != workmgmt.CanonicalStateInProgress {
		t.Errorf("run_started canonical = %q, want in_progress", fp.calls[0].CanonicalState)
	}
	if !containsState(fp.calls[0].ExpectedSourceStates, workmgmt.CanonicalStateUpNext) {
		t.Errorf("run_started expected sources = %v, want to contain up_next", fp.calls[0].ExpectedSourceStates)
	}
}

// newCampaignStartServerPG wires a Server with a REAL Postgres campaign + run
// repo (so the run-link FK is enforced and RestartCampaignItem executes against
// the adapter), a recording audit repo, and the GitHub spec stub — the setup
// the restart cross-boundary E2E needs to cross HTTP → engine → Postgres repo.
func newCampaignStartServerPG(t *testing.T) (*Server, campaign.Repository, run.Repository, *campaignAuditRecorder) {
	t.Helper()
	pool := pgtest.NewPool(t)
	campaigns := campaign.NewPostgresRepository(pool)
	runs := run.NewPostgresRepository(pool)
	aud := &campaignAuditRecorder{}
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	ghSrv := fake.server(t)
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
	s := New(Config{Addr: "127.0.0.1:0", CampaignRepo: campaigns, RunRepo: runs, AuditRepo: aud, GitHub: gh})
	return s, campaigns, runs, aud
}

// TestStartCampaignItemRun_RestartCancelled_CrossBoundary_E2E is the E32.9
// (#1729) headline done-means over a real Postgres: a terminal-cancelled item
// with satisfied deps and a Blocked dependent — the campaign-wedge shape — is
// restarted via POST /v0/campaigns/{id}/runs. It asserts the full crossing:
//   - status reports next_action=start_run on the cancelled ref (folded into the
//     wire cancelled slice), with the dependent still blocked;
//   - the POST resets the item, mints + re-links a FRESH run (run_id changes),
//     moves the item to running, and emits exactly one campaign_issue_restarted;
//   - a second POST while the item is running is refused 409 item_not_eligible
//     (the running-item refusal is preserved);
//   - settling the fresh run succeeded unblocks the dependent on the next read.
func TestStartCampaignItemRun_RestartCancelled_CrossBoundary_E2E(t *testing.T) {
	ctx := context.Background()
	s, campaigns, runs, aud := newCampaignStartServerPG(t)

	c, err := campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo: "kuhlman-labs/fishhawk", EpicRef: "issue:99",
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	// A: no deps — driven to succeeded so B's deps are satisfied.
	itemA, err := campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:100"})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := campaigns.TransitionCampaignItem(ctx, itemA.ID, campaign.ItemStateSucceeded); err != nil {
		t.Fatalf("A→succeeded: %v", err)
	}
	// B: depends on A — driven to cancelled with a linked (now stale) run.
	itemB, err := campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:101", DependsOn: []string{"issue:100"}})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	staleRef := "issue:101"
	staleRun, err := runs.CreateRun(ctx, run.CreateRunParams{Repo: c.Repo, WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerGitHubIssue, TriggerRef: &staleRef})
	if err != nil {
		t.Fatalf("create stale run: %v", err)
	}
	if _, err := campaigns.SetCampaignItemRun(ctx, itemB.ID, &staleRun.ID); err != nil {
		t.Fatalf("link B stale run: %v", err)
	}
	if _, err := campaigns.TransitionCampaignItem(ctx, itemB.ID, campaign.ItemStateRunning); err != nil {
		t.Fatalf("B→running: %v", err)
	}
	if _, err := campaigns.TransitionCampaignItem(ctx, itemB.ID, campaign.ItemStateCancelled); err != nil {
		t.Fatalf("B→cancelled: %v", err)
	}
	// C: depends on B — stays pending (blocked on B).
	if _, err := campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:102", DependsOn: []string{"issue:101"}}); err != nil {
		t.Fatalf("create C: %v", err)
	}
	// Campaign running (DeriveState over succeeded/cancelled/pending), so the
	// dispatch gate passes.
	if _, err := campaigns.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("campaign→running: %v", err)
	}

	// Status: the cancelled item is restartable → start_run on issue:101, folded
	// into the wire cancelled slice; C still blocked.
	st := getCampaignStatusBody(t, s, c.ID)
	if st.NextAction.Action != "start_run" || st.NextAction.IssueRef != "issue:101" {
		t.Fatalf("next_action = %+v, want start_run issue:101", st.NextAction)
	}
	if !containsRef(st.Rollup.Cancelled, "issue:101") {
		t.Errorf("rollup.Cancelled = %v, want it to fold in issue:101", st.Rollup.Cancelled)
	}
	if !containsRef(st.Rollup.Blocked, "issue:102") {
		t.Errorf("rollup.Blocked = %v, want [issue:102] still blocked", st.Rollup.Blocked)
	}

	// Restart via POST /runs.
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:101","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("restart status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Item.State != string(campaign.ItemStateRunning) {
		t.Errorf("item state = %q, want running", body.Item.State)
	}
	if body.Item.RunID == nil || *body.Item.RunID != body.Run.ID {
		t.Errorf("item run_id = %v, want linked to fresh run %s", body.Item.RunID, body.Run.ID)
	}
	if body.Run.ID == staleRun.ID {
		t.Errorf("fresh run id = %s, want a NEW run distinct from the stale %s", body.Run.ID, staleRun.ID)
	}
	if n := aud.count("campaign_issue_restarted"); n != 1 {
		t.Errorf("campaign_issue_restarted count = %d, want exactly 1", n)
	}
	if n := aud.count("campaign_issue_started"); n != 1 {
		t.Errorf("campaign_issue_started count = %d, want 1 (fresh run started)", n)
	}

	// Regression: a second POST while the item is running is refused.
	w2 := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:101","workflow_id":"feature_change","runner_kind":"local"}`)
	if w2.Code != http.StatusConflict || decodeCampaignError(t, w2) != "item_not_eligible" {
		t.Fatalf("second start = %d/%s, want 409 item_not_eligible (body=%s)", w2.Code, decodeCampaignError(t, w2), w2.Body.String())
	}

	// Settle the fresh run succeeded → reconcile-on-read settles B and unblocks C.
	// A real run reaches a terminal state through running.
	if _, err := runs.TransitionRun(ctx, body.Run.ID, run.StateRunning); err != nil {
		t.Fatalf("fresh run→running: %v", err)
	}
	if _, err := runs.TransitionRun(ctx, body.Run.ID, run.StateSucceeded); err != nil {
		t.Fatalf("settle fresh run: %v", err)
	}
	st2 := getCampaignStatusBody(t, s, c.ID)
	if !containsRef(st2.Rollup.Done, "issue:101") {
		t.Errorf("rollup.Done = %v, want it to include the settled issue:101", st2.Rollup.Done)
	}
	if !containsRef(st2.Rollup.Eligible, "issue:102") {
		t.Errorf("rollup.Eligible = %v, want [issue:102] (dependent unblocked)", st2.Rollup.Eligible)
	}
	if st2.NextAction.Action != "start_run" || st2.NextAction.IssueRef != "issue:102" {
		t.Errorf("next_action = %+v, want start_run issue:102 after unblock", st2.NextAction)
	}
}

// seedRestartableCampaign seeds a running campaign with a single deps-satisfied,
// non-human-led CANCELLED item — the shape campaign.NextEligible classifies as
// Restartable (engine.go: a cancelled item with satisfied deps and Autonomy!=
// "low" diverts to Restartable). It returns the campaign and the item pointer so
// a test can drive handleStartCampaignItemRun's `if isRestartable` restart branch
// and inspect the item's post-call state. The fake's default campaign state is
// running, so the dispatch gate passes.
func seedRestartableCampaign(crepo *fakeCampaignRepo) (*campaign.Campaign, *campaign.Item) {
	item := cItem("issue:100", nil, campaign.ItemStateCancelled)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{item})
	return c, item
}

// TestStartCampaignItemRun_RestartInvalidTransition_409 pins the first restart
// error arm: RestartCampaignItem returning campaign.InvalidTransitionError (a
// concurrent restart/dispatch raced the item off its terminal state) maps to 409
// item_not_eligible via the errors.As(rerr,&inv) case. Injected through the fake's
// restartErr seam on a NextEligible-classified Restartable item so control reaches
// the `if isRestartable` block.
func TestStartCampaignItemRun_RestartInvalidTransition_409(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, _ := newCampaignStartServer(t, crepo)
	c, _ := seedRestartableCampaign(crepo)
	crepo.restartErr = campaign.InvalidTransitionError{Kind: "campaign_item", From: "cancelled", To: "pending"}

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "item_not_eligible" {
		t.Errorf("error code = %q, want item_not_eligible", code)
	}
	if crepo.restartCalls != 1 {
		t.Errorf("restartCalls = %d, want 1 (restart branch reached)", crepo.restartCalls)
	}
}

// TestStartCampaignItemRun_RestartNotFound_404 pins the second restart error arm:
// RestartCampaignItem returning campaign.ErrNotFound maps to 404
// campaign_item_not_found via the errors.Is(rerr, campaign.ErrNotFound) case.
func TestStartCampaignItemRun_RestartNotFound_404(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, _ := newCampaignStartServer(t, crepo)
	c, _ := seedRestartableCampaign(crepo)
	crepo.restartErr = campaign.ErrNotFound

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "campaign_item_not_found" {
		t.Errorf("error code = %q, want campaign_item_not_found", code)
	}
	if crepo.restartCalls != 1 {
		t.Errorf("restartCalls = %d, want 1 (restart branch reached)", crepo.restartCalls)
	}
}

// TestStartCampaignItemRun_RestartInternalError_500 pins the default restart error
// arm: a plain (non-InvalidTransition, non-ErrNotFound) error from
// RestartCampaignItem maps to 500 internal_error via the default case.
func TestStartCampaignItemRun_RestartInternalError_500(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, _ := newCampaignStartServer(t, crepo)
	c, _ := seedRestartableCampaign(crepo)
	crepo.restartErr = errInjected

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", code)
	}
	if crepo.restartCalls != 1 {
		t.Errorf("restartCalls = %d, want 1 (restart branch reached)", crepo.restartCalls)
	}
}

// TestStartCampaignItemRun_RestartThenDownstreamFailure_ReAdmittable pins the
// partial-failure edge: RestartCampaignItem succeeds (item reset to pending, run
// link cleared, campaign_issue_restarted emitted exactly once) but the immediate
// downstream SetCampaignItemRun link fails, leaving the item PENDING and UNLINKED
// (RunID nil). A subsequent verb call must re-admit that item via the Eligible
// pending path (NOT a second restart) and drive it to running — demonstrating the
// pending-unlinked item is retryable rather than wedged.
func TestStartCampaignItemRun_RestartThenDownstreamFailure_ReAdmittable(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, aud := newCampaignStartServer(t, crepo)
	c, item := seedRestartableCampaign(crepo)
	// The post-restart run-link write fails, aborting after RestartCampaignItem
	// already reset the item.
	crepo.setRunErr = errInjected

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("first start = %d, want 500 (link failure; body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "internal_error" {
		t.Errorf("error code = %q, want internal_error", code)
	}
	if crepo.restartCalls != 1 {
		t.Errorf("restartCalls = %d, want 1 (item restarted once)", crepo.restartCalls)
	}
	if n := aud.count("campaign_issue_restarted"); n != 1 {
		t.Errorf("campaign_issue_restarted count = %d, want exactly 1", n)
	}
	// The item is left pending and unlinked — the state the re-admittability path
	// must recover from.
	if item.State != campaign.ItemStatePending {
		t.Errorf("item state = %q, want pending (reset but unlinked)", item.State)
	}
	if item.RunID != nil {
		t.Errorf("item run_id = %v, want nil (link failed)", item.RunID)
	}

	// Recover: clear the injected link failure and re-issue the verb. The now-pending
	// item is Eligible, so it is admitted WITHOUT a second restart.
	crepo.setRunErr = nil
	w2 := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w2.Code != http.StatusCreated {
		t.Fatalf("re-admit start = %d, want 201 (body=%s)", w2.Code, w2.Body.String())
	}
	var body startItemRunBody
	if err := json.Unmarshal(w2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Item.State != string(campaign.ItemStateRunning) {
		t.Errorf("item state = %q, want running", body.Item.State)
	}
	if body.Item.RunID == nil || *body.Item.RunID != body.Run.ID {
		t.Errorf("item run_id = %v, want linked to %s", body.Item.RunID, body.Run.ID)
	}
	if crepo.restartCalls != 1 {
		t.Errorf("restartCalls = %d, want 1 (re-admitted via Eligible path, NOT a second restart)", crepo.restartCalls)
	}
}

// seedRecoveryChild mints a run in the fake run repo carrying parent_run_id =
// parentID (a resume/recovery child, the #216 lineage) and drives it to state.
// It models the run fishhawk_resume_run mints when an operator recovers a
// failed run (#1751).
func seedRecoveryChild(t *testing.T, rrepo *fakeRepo, parentID uuid.UUID, state run.State) *run.Run {
	t.Helper()
	child, err := rrepo.CreateRun(context.Background(), run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "sha-rec",
		TriggerSource: run.TriggerCLI,
		ParentRunID:   &parentID,
	})
	if err != nil {
		t.Fatalf("seed recovery child: %v", err)
	}
	if state != run.StatePending {
		if _, err := rrepo.TransitionRun(context.Background(), child.ID, state); err != nil {
			t.Fatalf("transition recovery child to %s: %v", state, err)
		}
	}
	return child
}

// startFailedItemRun starts an item run, flips the linked run to failed, and
// returns the campaign + the now-failed run id — the shared setup for the
// recovery-lineage reconcile tests (#1751).
func startFailedItemRun(t *testing.T, s *Server, rrepo *fakeRepo, crepo *fakeCampaignRepo) (*campaign.Campaign, uuid.UUID) {
	t.Helper()
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, err := rrepo.TransitionRun(context.Background(), body.Run.ID, run.StateFailed); err != nil {
		t.Fatalf("flip run failed: %v", err)
	}
	return c, body.Run.ID
}

// TestReconcileOnRead_FailedRun_RecoveredChildSucceeded_E2E is the headline
// done-means of #1751: an item whose linked run failed category-B but was
// recovered via resume_run (a succeeded recovery child) settles SUCCEEDED — not
// failed — the campaign derives succeeded, and the item re-links to the
// recovery child for provenance.
func TestReconcileOnRead_FailedRun_RecoveredChildSucceeded_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c, failedRunID := startFailedItemRun(t, s, rrepo, crepo)

	// The operator recovered the failed run via resume_run; the recovery child
	// succeeded and merged.
	child := seedRecoveryChild(t, rrepo, failedRunID, run.StateSucceeded)

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 1 || st.Rollup.Done[0] != "issue:100" {
		t.Errorf("rollup.Done = %v, want [issue:100] (settled off recovery child)", st.Rollup.Done)
	}
	if len(st.Rollup.Failed) != 0 {
		t.Errorf("rollup.Failed = %v, want empty (recovery succeeded)", st.Rollup.Failed)
	}
	if st.Campaign.State != string(campaign.StateSucceeded) {
		t.Errorf("campaign state = %q, want succeeded", st.Campaign.State)
	}
	if st.NextAction.Action != "complete" {
		t.Errorf("next_action = %+v, want complete", st.NextAction)
	}
	// The item is re-linked to the recovery child that produced the outcome.
	if len(st.Items) != 1 || st.Items[0].RunID == nil || *st.Items[0].RunID != child.ID {
		t.Errorf("item run_id = %v, want re-linked to recovery child %s", st.Items[0].RunID, child.ID)
	}
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Errorf("campaign_issue_settled count = %d, want 1", n)
	}
}

// TestReconcileOnRead_FailedRun_RecoveryInFlight_LeavesRunning covers the
// in-flight-descendant branch: a failed run with a still-running recovery child
// leaves the item RUNNING (not settled), so a later read re-settles it off the
// recovery's terminal outcome. Binding condition: the in-flight branch must not
// settle the item.
func TestReconcileOnRead_FailedRun_RecoveryInFlight_LeavesRunning(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c, failedRunID := startFailedItemRun(t, s, rrepo, crepo)

	seedRecoveryChild(t, rrepo, failedRunID, run.StateRunning) // non-terminal recovery

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Running) != 1 || st.Rollup.Running[0] != "issue:100" {
		t.Errorf("rollup.Running = %v, want [issue:100] (recovery in flight)", st.Rollup.Running)
	}
	if len(st.Rollup.Failed) != 0 {
		t.Errorf("rollup.Failed = %v, want empty (not settled while recovering)", st.Rollup.Failed)
	}
	if st.NextAction.Action != "wait" {
		t.Errorf("next_action = %+v, want wait", st.NextAction)
	}
	if st.Items[0].State != string(campaign.ItemStateRunning) {
		t.Errorf("item state = %q, want running (in-flight recovery)", st.Items[0].State)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled count = %d, want 0 (nothing settled)", n)
	}
}

// TestReconcileOnRead_FailedRun_RecoveryAlsoFailed_StaysFailed is the
// regression guard: a failed run whose newest terminal recovery descendant also
// failed (no in-flight child) settles FAILED, exactly as today, and the
// single-item campaign derives failed (the #1838 anti-over-fix guard — no
// actionable sibling remains). The settled item is deps-satisfied and
// non-human-led, so it is restartable (folded into the wire cancelled slice) and
// next_action is start_run.
func TestReconcileOnRead_FailedRun_RecoveryAlsoFailed_StaysFailed(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c, failedRunID := startFailedItemRun(t, s, rrepo, crepo)

	child := seedRecoveryChild(t, rrepo, failedRunID, run.StateFailed)

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Failed) != 0 {
		t.Errorf("rollup.Failed = %v, want [] (restartable item folded into cancelled)", st.Rollup.Failed)
	}
	if len(st.Rollup.Cancelled) != 1 || st.Rollup.Cancelled[0] != "issue:100" {
		t.Errorf("rollup.Cancelled = %v, want [issue:100] (restartable folded)", st.Rollup.Cancelled)
	}
	if st.Campaign.State != string(campaign.StateFailed) {
		t.Errorf("campaign state = %q, want failed", st.Campaign.State)
	}
	if st.NextAction.Action != "start_run" {
		t.Errorf("next_action = %+v, want start_run (restartable item)", st.NextAction)
	}
	// Settled off — and re-linked to — the newest terminal recovery descendant.
	if st.Items[0].RunID == nil || *st.Items[0].RunID != child.ID {
		t.Errorf("item run_id = %v, want re-linked to newest terminal descendant %s", st.Items[0].RunID, child.ID)
	}
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Errorf("campaign_issue_settled count = %d, want 1", n)
	}
}

// TestReconcileOnRead_FailedRun_RecoveryListError_SettlesFailed covers the
// best-effort ListRuns-error branch in newestTerminalRecoveryDescendant: a
// ListRuns failure while walking recovery descendants degrades to (nil,false)
// so the item settles off the failed run (today's behavior) and the read still
// returns 200.
func TestReconcileOnRead_FailedRun_RecoveryListError_SettlesFailed(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, _ := newCampaignStartServer(t, crepo)
	c, _ := startFailedItemRun(t, s, rrepo, crepo)

	rrepo.listErr = errInjected // recovery-descendant walk errors

	st := getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	// The item settled off the failed run (fell back to today's failed settle);
	// being deps-satisfied + non-human-led it is restartable, folded into the wire
	// cancelled slice, and the single-item campaign derives failed.
	if len(st.Rollup.Failed) != 0 {
		t.Errorf("rollup.Failed = %v, want [] (restartable item folded into cancelled)", st.Rollup.Failed)
	}
	if len(st.Rollup.Cancelled) != 1 || st.Rollup.Cancelled[0] != "issue:100" {
		t.Errorf("rollup.Cancelled = %v, want [issue:100] (restartable folded)", st.Rollup.Cancelled)
	}
	if st.Campaign.State != string(campaign.StateFailed) {
		t.Errorf("campaign state = %q, want failed", st.Campaign.State)
	}
}

// TestReconcileOnRead_FailedRun_RelinkError_StillSettles covers the best-effort
// relink branch: a succeeded recovery descendant settles the item succeeded
// even when SetCampaignItemRun (the provenance re-link) fails — the failed link
// write is logged, never unwinds the settle, and the read returns 200.
func TestReconcileOnRead_FailedRun_RelinkError_StillSettles(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, _ := newCampaignStartServer(t, crepo)
	c, failedRunID := startFailedItemRun(t, s, rrepo, crepo)

	seedRecoveryChild(t, rrepo, failedRunID, run.StateSucceeded)
	crepo.setRunErr = errInjected // relink write fails

	st := getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	if st.Items[0].State != string(campaign.ItemStateSucceeded) {
		t.Errorf("item state = %q, want succeeded despite relink failure", st.Items[0].State)
	}
	if st.Campaign.State != string(campaign.StateSucceeded) {
		t.Errorf("campaign state = %q, want succeeded", st.Campaign.State)
	}
}

// TestNewestTerminalRecoveryDescendant_NilRunRepo covers the defensive
// nil-RunRepo guard (unreachable from reconcile pass 1, which is itself gated on
// RunRepo != nil, but guarded for direct callers): it returns (nil,false).
func TestNewestTerminalRecoveryDescendant_NilRunRepo(t *testing.T) {
	s := New(Config{})
	desc, inFlight := s.newestTerminalRecoveryDescendant(context.Background(), uuid.New())
	if desc != nil || inFlight {
		t.Errorf("nil RunRepo = (%v,%v), want (nil,false)", desc, inFlight)
	}
}

// TestNewestTerminalRecoveryDescendant_CycleGuard proves the visited-set guard
// terminates on a corrupt parent_run_id cycle (root ↔ child) instead of looping
// forever, returning the terminal child.
func TestNewestTerminalRecoveryDescendant_CycleGuard(t *testing.T) {
	rrepo := newFakeRepo()
	ctx := context.Background()
	root, err := rrepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "s", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := rrepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "s", TriggerSource: run.TriggerCLI,
		ParentRunID: &root.ID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// Introduce a cycle: root also points at child as its parent.
	root.ParentRunID = &child.ID
	if _, err := rrepo.TransitionRun(ctx, child.ID, run.StateFailed); err != nil {
		t.Fatalf("transition child: %v", err)
	}
	s := New(Config{RunRepo: rrepo})
	desc, inFlight := s.newestTerminalRecoveryDescendant(ctx, root.ID)
	if inFlight {
		t.Errorf("inFlight = true, want false (child is terminal)")
	}
	if desc == nil || desc.ID != child.ID {
		t.Errorf("desc = %v, want terminal child %s", desc, child.ID)
	}
}

// TestMapRunTerminalToItemState covers all three run-terminal states map to the
// corresponding item-terminal state, and a non-terminal state is unmapped.
func TestMapRunTerminalToItemState(t *testing.T) {
	cases := []struct {
		in   run.State
		want campaign.ItemState
		ok   bool
	}{
		{run.StateSucceeded, campaign.ItemStateSucceeded, true},
		{run.StateFailed, campaign.ItemStateFailed, true},
		{run.StateCancelled, campaign.ItemStateCancelled, true},
		{run.StateRunning, "", false},
		{run.StatePending, "", false},
	}
	for _, tc := range cases {
		got, ok := mapRunTerminalToItemState(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("mapRunTerminalToItemState(%s) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestStartCampaignItemRun_Blocked_NamesUnmetDependency refuses a blocked item
// 409 item_not_eligible and names the first unmet dependency.
func TestStartCampaignItemRun_Blocked_NamesUnmetDependency(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
		cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending),
	})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:101","workflow_id":"feature_change"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "item_not_eligible" {
		t.Errorf("code = %q, want item_not_eligible", code)
	}
	if !strings.Contains(w.Body.String(), "issue:100") {
		t.Errorf("body should name unmet dependency issue:100: %s", w.Body.String())
	}
}

// TestStartCampaignItemRun_AlreadyRunning refuses an already-running item.
func TestStartCampaignItemRun_AlreadyRunning(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	rid := uuid.New()
	running := cItem("issue:100", nil, campaign.ItemStateRunning)
	running.RunID = &rid
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{running})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change"}`)
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "item_not_eligible" {
		t.Fatalf("status/code = %d/%s, want 409 item_not_eligible (body=%s)", w.Code, decodeCampaignError(t, w), w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "running") {
		t.Errorf("body should report the running state: %s", w.Body.String())
	}
}

// TestStartCampaignItemRun_AlreadyDone refuses a terminal (succeeded) item.
func TestStartCampaignItemRun_AlreadyDone(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStateSucceeded),
	})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change"}`)
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "item_not_eligible" {
		t.Fatalf("status/code = %d/%s, want 409 item_not_eligible", w.Code, decodeCampaignError(t, w))
	}
	if !strings.Contains(w.Body.String(), "succeeded") {
		t.Errorf("body should report the succeeded state: %s", w.Body.String())
	}
}

// TestStartCampaignItemRun_HumanLed_NamesHumanLedReason refuses a deps-satisfied
// autonomy:low (human-led) item with the DISTINCT 409 item_human_led code whose
// detail names the human-led reason and does NOT tell the caller to start a ref
// (#1697). This is the done-means: a no-op edit that left the generic
// item_not_eligible code fails here.
func TestStartCampaignItemRun_HumanLed_NamesHumanLedReason(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	humanLed := cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending)
	humanLed.Autonomy = "low"
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStateSucceeded),
		humanLed,
	})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:101","workflow_id":"feature_change"}`)
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "item_human_led" {
		t.Fatalf("status/code = %d/%s, want 409 item_human_led (body=%s)", w.Code, decodeCampaignError(t, w), w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "human-led") {
		t.Errorf("body should name the human-led reason: %s", body)
	}
	if !strings.Contains(body, "attend_human_led") {
		t.Errorf("body should reference attend_human_led: %s", body)
	}
	if strings.Contains(body, "start the ref") || strings.Contains(body, "next_action names") {
		t.Errorf("body must NOT tell the caller to start a ref: %s", body)
	}
}

// TestStartCampaignItemRun_ItemNotFound 404s an unknown issue_ref.
func TestStartCampaignItemRun_ItemNotFound(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:999","workflow_id":"feature_change"}`)
	if w.Code != http.StatusNotFound || decodeCampaignError(t, w) != "campaign_item_not_found" {
		t.Fatalf("status/code = %d/%s, want 404 campaign_item_not_found", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_CampaignNotFound 404s an unknown campaign.
func TestStartCampaignItemRun_CampaignNotFound(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	w := postStartItemRun(t, s, uuid.New(), `{"issue_ref":"issue:100","workflow_id":"feature_change"}`)
	if w.Code != http.StatusNotFound || decodeCampaignError(t, w) != "campaign_not_found" {
		t.Fatalf("status/code = %d/%s, want 404 campaign_not_found", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_PausedCampaign_NotStartable 409s when the campaign is paused.
func TestStartCampaignItemRun_PausedCampaign_NotStartable(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StatePaused
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change"}`)
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "campaign_not_startable" {
		t.Fatalf("status/code = %d/%s, want 409 campaign_not_startable", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_TerminalCampaign_NotStartable 409s when the campaign
// is terminal (succeeded).
func TestStartCampaignItemRun_TerminalCampaign_NotStartable(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStateSucceeded),
	})
	c.State = campaign.StateSucceeded
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change"}`)
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "campaign_not_startable" {
		t.Fatalf("status/code = %d/%s, want 409 campaign_not_startable", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_BadRunnerKind 400s an unrecognized runner_kind.
func TestStartCampaignItemRun_BadRunnerKind(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"bogus"}`)
	if w.Code != http.StatusBadRequest || decodeCampaignError(t, w) != "validation_failed" {
		t.Fatalf("status/code = %d/%s, want 400 validation_failed", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_MissingFields 400s an empty issue_ref or workflow_id.
func TestStartCampaignItemRun_MissingFields(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	for _, body := range []string{
		`{"workflow_id":"feature_change"}`,
		`{"issue_ref":"issue:100"}`,
	} {
		w := postStartItemRun(t, s, c.ID, body)
		if w.Code != http.StatusBadRequest || decodeCampaignError(t, w) != "validation_failed" {
			t.Errorf("body %s: status/code = %d/%s, want 400 validation_failed", body, w.Code, decodeCampaignError(t, w))
		}
	}
}

// TestStartCampaignItemRun_MissingScope 403s a token lacking write:campaigns.
func TestStartCampaignItemRun_MissingScope(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+c.ID.String()+"/runs",
		strings.NewReader(`{"issue_ref":"issue:100","workflow_id":"feature_change"}`))
	req.SetPathValue("campaign_id", c.ID.String())
	id := Identity{Subject: "token:x", TokenID: "tok-1", Scopes: []string{"write:runs"}}
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	w := httptest.NewRecorder()
	s.handleStartCampaignItemRun(w, req)
	if w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "insufficient_scope" {
		t.Fatalf("status/code = %d/%s, want 403 insufficient_scope (body=%s)", w.Code, decodeCampaignError(t, w), w.Body.String())
	}
}

// TestStartCampaignItemRun_NilCampaignRepo 503s when no campaign repo is wired,
// before the write-scope check (the 503-vs-401 idiom).
func TestStartCampaignItemRun_NilCampaignRepo(t *testing.T) {
	s := New(Config{})
	w := postStartItemRun(t, s, uuid.New(), `{"issue_ref":"issue:100","workflow_id":"feature_change"}`)
	if w.Code != http.StatusServiceUnavailable || decodeCampaignError(t, w) != "campaign_repo_unconfigured" {
		t.Fatalf("status/code = %d/%s, want 503 campaign_repo_unconfigured", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_TransitionFailure_RollsBackLink asserts a failed
// running transition after the link committed rolls the link back (so the item
// re-partitions as Eligible) and surfaces 500.
func TestStartCampaignItemRun_TransitionFailure_RollsBackLink(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, _ := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning
	crepo.transItemErr = errInjected
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
	// The item was unlinked on rollback so a retry re-partitions it Eligible.
	it := crepo.itemsByCmp[c.ID][0]
	if it.RunID != nil {
		t.Errorf("item run_id = %v, want nil after rollback", it.RunID)
	}
}

// errInjected is a sentinel for the repo error-injection tests.
var errInjected = fmt.Errorf("injected repo failure")

// TestStartCampaignItemRun_BadCampaignID 400s a non-UUID path value.
func TestStartCampaignItemRun_BadCampaignID(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/not-a-uuid/runs",
		strings.NewReader(`{"issue_ref":"issue:1","workflow_id":"feature_change"}`))
	req.SetPathValue("campaign_id", "not-a-uuid")
	w := httptest.NewRecorder()
	s.handleStartCampaignItemRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest || decodeCampaignError(t, w) != "validation_failed" {
		t.Fatalf("status/code = %d/%s, want 400 validation_failed", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_BadBody 400s an unknown field (DisallowUnknownFields).
func TestStartCampaignItemRun_BadBody(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s := New(Config{CampaignRepo: crepo})
	c := crepo.seedCampaignWithItems("x/y", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","bogus":true}`)
	if w.Code != http.StatusBadRequest || decodeCampaignError(t, w) != "validation_failed" {
		t.Fatalf("status/code = %d/%s, want 400 validation_failed", w.Code, decodeCampaignError(t, w))
	}
}

// TestStartCampaignItemRun_RunStartFails_BadGateway 502s when
// StartRunForCampaignIssue cannot resolve the installation/spec.
func TestStartCampaignItemRun_RunStartFails_BadGateway(t *testing.T) {
	crepo := newFakeCampaignRepo()
	// GitHub stub whose installation endpoint 404s → resolve installation fails.
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	fake.installationStatus = http.StatusNotFound
	fake.installationBody = `{"message":"Not Found"}`
	ghSrv := fake.server(t)
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), GitHub: gh})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusBadGateway || decodeCampaignError(t, w) != "campaign_run_start_failed" {
		t.Fatalf("status/code = %d/%s, want 502 campaign_run_start_failed (body=%s)", w.Code, decodeCampaignError(t, w), w.Body.String())
	}
	// The item must be left un-started (no run linked) for a retry.
	if it := crepo.itemsByCmp[c.ID][0]; it.RunID != nil || it.State != campaign.ItemStatePending {
		t.Errorf("item = {run_id:%v state:%s}, want unlinked + pending after failed start", it.RunID, it.State)
	}
}

// TestStartCampaignItemRun_LinkFails_500 covers a SetCampaignItemRun failure
// after the run minted → 500 (the link step's error branch).
func TestStartCampaignItemRun_LinkFails_500(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, _ := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	crepo.setRunErr = errInjected
	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
}

// TestStartCampaignItemRun_TransitionInvalid_409 covers the start handler's
// errors.As(err, &inv) arm: an InvalidTransitionError from the running
// transition maps to 409 invalid_transition (not the generic 500) and STILL
// rolls the item-run link back so the item re-partitions Eligible on retry.
// TestStartCampaignItemRun_TransitionFailure_RollsBackLink injects a plain
// error (the 500 arm); this pins the typed-conflict arm.
func TestStartCampaignItemRun_TransitionInvalid_409(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, _, _ := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning
	crepo.transItemErr = campaign.InvalidTransitionError{Kind: "campaign_item", From: "pending", To: "running"}

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if code := decodeCampaignError(t, w); code != "invalid_transition" {
		t.Errorf("code = %q, want invalid_transition", code)
	}
	// The link was rolled back so the item re-partitions Eligible on retry.
	if it := crepo.itemsByCmp[c.ID][0]; it.RunID != nil {
		t.Errorf("item run_id = %v, want nil after rollback", it.RunID)
	}
}

// TestStartCampaignItemRun_ReconcileOnRead_GetRunError_StillReturns200 covers
// the reconcile GetRun-failure swallow path: a linked run whose GetRun errors is
// logged-and-swallowed, the item is left running, NO settle audit is emitted,
// and the status read still returns 200 (best-effort — never fails the read).
func TestStartCampaignItemRun_ReconcileOnRead_GetRunError_StillReturns200(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}

	// GetRun now errors: reconcile logs-and-swallows, leaving the item running.
	rrepo.getErr = errInjected
	st := getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	if len(st.Rollup.Running) != 1 || st.Rollup.Running[0] != "issue:100" {
		t.Errorf("rollup.Running = %v, want [issue:100] (item left running on GetRun error)", st.Rollup.Running)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (nothing settled on GetRun error)", n)
	}
}

// TestStartCampaignItemRun_ReconcileOnRead_SettleTransitionError_StillReturns200
// covers the reconcile settle-transition-failure swallow path: when the item's
// terminal-run settle TransitionCampaignItem fails, the error is
// logged-and-swallowed, the item is left running, no settle audit is emitted,
// and the read still returns 200.
func TestStartCampaignItemRun_ReconcileOnRead_SettleTransitionError_StillReturns200(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, err := rrepo.TransitionRun(context.Background(), body.Run.ID, run.StateSucceeded); err != nil {
		t.Fatalf("flip run terminal: %v", err)
	}

	// The settle transition now fails: reconcile logs-and-swallows, item stays running.
	crepo.transItemErr = errInjected
	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Running) != 1 || st.Rollup.Running[0] != "issue:100" {
		t.Errorf("rollup.Running = %v, want [issue:100] (settle failed, item left running)", st.Rollup.Running)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (settle transition failed)", n)
	}
}

// TestStartCampaignItemRun_ReconcileOnRead_DeriveTransitionError_StillReturns200
// covers the reconcile derive-failure swallow path: the item settles (done) but
// the campaign derivation transition fails — logged-and-swallowed — leaving the
// campaign un-advanced (still running) while the read still returns 200 and the
// settle is not unwound.
func TestStartCampaignItemRun_ReconcileOnRead_DeriveTransitionError_StillReturns200(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, err := rrepo.TransitionRun(context.Background(), body.Run.ID, run.StateSucceeded); err != nil {
		t.Fatalf("flip run terminal: %v", err)
	}

	// The item settle succeeds but the campaign derivation transition fails.
	crepo.transCmpErr = errInjected
	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 1 || st.Rollup.Done[0] != "issue:100" {
		t.Errorf("rollup.Done = %v, want [issue:100] (item settled despite derive failure)", st.Rollup.Done)
	}
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Errorf("campaign_issue_settled = %d, want 1 (item settled)", n)
	}
	if n := aud.count("campaign_advanced"); n != 0 {
		t.Errorf("campaign_advanced = %d, want 0 (derive transition failed)", n)
	}
	// The campaign was left un-advanced; the failed transition did not unwind the
	// settle and the read still returned 200 — the next read re-derives.
	if got := crepo.campaigns[c.ID].State; got != campaign.StateRunning {
		t.Errorf("campaign state = %q, want running (derive transition failed, left for next read)", got)
	}
}

// TestStartCampaignItemRun_ReconcileOnRead_RelistError_StillReturns200 covers
// the reconcile re-list-after-settle swallow path: the item settles but the
// re-list that feeds campaign derivation fails — logged-and-swallowed — so the
// campaign is not re-derived (no advance) yet the read still returns 200. The
// read's initial list and the post-reconcile refresh still succeed (only the
// reconcile re-list, the 2nd list of the read, is failed).
func TestStartCampaignItemRun_ReconcileOnRead_RelistError_StillReturns200(t *testing.T) {
	crepo := newFakeCampaignRepo()
	s, rrepo, aud := newCampaignStartServer(t, crepo)
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	w := postStartItemRun(t, s, c.ID, `{"issue_ref":"issue:100","workflow_id":"feature_change","runner_kind":"local"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("start status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var body startItemRunBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, err := rrepo.TransitionRun(context.Background(), body.Run.ID, run.StateSucceeded); err != nil {
		t.Fatalf("flip run terminal: %v", err)
	}

	// Reset the counter so it gates only the read's list calls, and fail the 2nd
	// one (reconcile's re-list-after-settle). The initial list (1) and the
	// handler's post-reconcile refresh (3) still succeed.
	crepo.itemsCalls = 0
	crepo.itemsErrOnCall = 2
	_ = getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Errorf("campaign_issue_settled = %d, want 1 (item settled before the re-list failed)", n)
	}
	// Derivation was skipped because its re-list failed → no advance emitted.
	if n := aud.count("campaign_advanced"); n != 0 {
		t.Errorf("campaign_advanced = %d, want 0 (re-list failed, derivation skipped)", n)
	}
}

// --- run-less issue-closed settle pass (#1558) ---

// runlessIssue is a configurable GitHub issue response for the run-less settle
// pass tests: State/StateReason are echoed by the issues endpoint.
type runlessIssue struct {
	state       string
	stateReason string
}

// runlessGitHub configures a GitHub stub serving GET .../installation and
// GET .../issues/{number} for the run-less issue-closed settle pass. issues maps
// issue number → response; issuesStatus, when non-zero, drives the issues
// endpoint to that HTTP status (the GetIssue-error branch). installCalls /
// issueCalls let a test assert the pass short-circuited (e.g. a non issue:N ref
// makes no GitHub call at all).
type runlessGitHub struct {
	mu            sync.Mutex
	installID     int64
	installStatus int // when non-zero, the installation endpoint returns this status
	issues        map[int]runlessIssue
	issuesStatus  int
	installCalls  int
	issueCalls    int
}

func newRunlessGitHubClient(t *testing.T, gh *runlessGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", func(w http.ResponseWriter, _ *http.Request) {
		gh.mu.Lock()
		gh.installCalls++
		id := gh.installID
		status := gh.installStatus
		gh.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
			return
		}
		fmt.Fprintf(w, `{"id":%d}`, id)
	})
	mux.HandleFunc("GET /repos/{owner}/{name}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		gh.mu.Lock()
		gh.issueCalls++
		status := gh.issuesStatus
		num, _ := strconv.Atoi(r.PathValue("number"))
		iss, known := gh.issues[num]
		gh.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
			return
		}
		if !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"number":%d,"state":%q,"state_reason":%q}`, num, iss.state, iss.stateReason)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// settledAuditPayload decodes the first campaign_issue_settled audit payload the
// recorder captured, so a test can assert the run-less variant's shape
// (settled_via=issue_closed, no run_id).
func settledAuditPayload(t *testing.T, aud *campaignAuditRecorder) map[string]any {
	t.Helper()
	aud.mu.Lock()
	defer aud.mu.Unlock()
	for _, e := range aud.entries {
		if e.Category == categoryCampaignIssueSettled {
			var p map[string]any
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode settled payload: %v", err)
			}
			return p
		}
	}
	t.Fatal("no campaign_issue_settled audit entry captured")
	return nil
}

// settledAuditPayloadsByRef returns every campaign_issue_settled payload keyed by
// its issue_ref, so a multi-item chain test can assert each item's audit carries
// the load-bearing settled_via / state_reason / outcome fields (not just that N
// entries exist). A missing or non-string issue_ref fails the test.
func settledAuditPayloadsByRef(t *testing.T, aud *campaignAuditRecorder) map[string]map[string]any {
	t.Helper()
	aud.mu.Lock()
	defer aud.mu.Unlock()
	byRef := map[string]map[string]any{}
	for _, e := range aud.entries {
		if e.Category != categoryCampaignIssueSettled {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode settled payload: %v", err)
		}
		ref, ok := p["issue_ref"].(string)
		if !ok {
			t.Fatalf("settled payload missing string issue_ref: %v", p)
		}
		byRef[ref] = p
	}
	return byRef
}

// TestReconcileOnRead_RunlessIssueClosed_SettlesAndUnblocks_E2E is the headline
// cross-boundary path for the run-less settle (#1558): a run-less, deps-satisfied
// item whose GitHub issue is closed-as-completed settles succeeded on a status
// read, emits a campaign_issue_settled(settled_via=issue_closed, no run_id)
// audit, and its dependent becomes eligible in the SAME response rollup +
// next_action — GitHub issue state → item transition → engine rollup → next_action.
func TestReconcileOnRead_RunlessIssueClosed_SettlesAndUnblocks_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"},
	}}
	gh := newRunlessGitHubClient(t, ghState)
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: gh})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
		cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 1 || st.Rollup.Done[0] != "issue:100" {
		t.Errorf("rollup.Done = %v, want [issue:100] after run-less settle", st.Rollup.Done)
	}
	if len(st.Rollup.Eligible) != 1 || st.Rollup.Eligible[0] != "issue:101" {
		t.Errorf("rollup.Eligible = %v, want [issue:101] (dependent unblocked)", st.Rollup.Eligible)
	}
	if st.NextAction.Action != "start_run" || st.NextAction.IssueRef != "issue:101" {
		t.Errorf("next_action = %+v, want start_run issue:101", st.NextAction)
	}
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Fatalf("campaign_issue_settled = %d, want 1", n)
	}
	// The run-less variant's payload: settled_via=issue_closed, state_reason,
	// outcome succeeded, and NO run_id.
	p := settledAuditPayload(t, aud)
	if p["settled_via"] != "issue_closed" {
		t.Errorf("settled_via = %v, want issue_closed", p["settled_via"])
	}
	if p["state_reason"] != "completed" {
		t.Errorf("state_reason = %v, want completed", p["state_reason"])
	}
	if p["outcome"] != "succeeded" {
		t.Errorf("outcome = %v, want succeeded", p["outcome"])
	}
	if _, ok := p["run_id"]; ok {
		t.Errorf("payload carries run_id = %v, want absent for a run-less settle", p["run_id"])
	}
	// Phase 1 resolves closed-status for EVERY run-less pending/blocked item
	// exactly once, independent of dep order (the dep gate moved to the in-memory
	// Phase-2 fixpoint), so both issue:100 (settled) and the dependent issue:101
	// (unknown → 404, skipped) are read once each — two calls, no re-read.
	if ghState.issueCalls != 2 {
		t.Errorf("issueCalls = %d, want 2 (both items read once in Phase 1)", ghState.issueCalls)
	}
	if ghState.installCalls != 1 {
		t.Errorf("installCalls = %d, want 1 (installation resolved once and cached)", ghState.installCalls)
	}
}

// TestReconcileOnRead_RunlessAllHumanLed_CampaignSucceeds_E2E is the BINDING
// condition (opus low): a fully run-less, ALL-human-led campaign whose every
// item's issue is closed-as-completed rolls up to StateSucceeded through the GET
// /status read path in ONE read — proving the new campaign pending→succeeded
// edge terminates the campaign end-to-end with zero dispatched runs.
func TestReconcileOnRead_RunlessAllHumanLed_CampaignSucceeds_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"},
		101: {state: "closed", stateReason: "completed"},
	}}
	gh := newRunlessGitHubClient(t, ghState)
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: gh})
	// Two INDEPENDENT run-less items (no depends_on): both deps-satisfied against
	// the initial done-set, so both settle in a single read.
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
		cItem("issue:101", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 2 {
		t.Errorf("rollup.Done = %v, want both items done", st.Rollup.Done)
	}
	if st.Campaign.State != string(campaign.StateSucceeded) {
		t.Errorf("campaign state = %q, want succeeded (pending→succeeded, run-less)", st.Campaign.State)
	}
	if st.NextAction.Action != "complete" {
		t.Errorf("next_action = %+v, want complete", st.NextAction)
	}
	if n := aud.count("campaign_issue_settled"); n != 2 {
		t.Errorf("campaign_issue_settled = %d, want 2", n)
	}
	if n := aud.count("campaign_advanced"); n != 1 {
		t.Errorf("campaign_advanced = %d, want 1 (pending→succeeded)", n)
	}
	if got := crepo.campaigns[c.ID].State; got != campaign.StateSucceeded {
		t.Errorf("persisted campaign state = %q, want succeeded", got)
	}
}

// TestReconcileOnRead_RunlessClosedChain_ConvergesInSingleRead_E2E is the #1758
// regression: a run-less child C (issue:101) depends_on a run-less child D
// (issue:100), BOTH closed-as-completed. The pass must settle the WHOLE chain in
// ONE status read regardless of iteration order. C is seeded FIRST so that the
// old single-hop pass (done-set computed once, deps gated in the same loop)
// would skip C — its dep D not yet in the done-set when C is visited — and leave
// C phantom-pending until a later read/restart; the in-memory Phase-2 fixpoint
// instead settles D, adds its ref, then settles C in the same read. Asserts both
// reach succeeded with nil run_id, each emits a settled_via=issue_closed audit,
// the eligible slice holds no CLOSED issue, next_action never targets a closed
// item, and the campaign advances to succeeded.
func TestReconcileOnRead_RunlessClosedChain_ConvergesInSingleRead_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"},
		101: {state: "closed", stateReason: "completed"},
	}}
	gh := newRunlessGitHubClient(t, ghState)
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: gh})
	// C (issue:101, depends_on D=issue:100) is seeded BEFORE D so it is iterated
	// first — proving the fixpoint converges independent of candidate order.
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending),
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StatePending

	st := getCampaignStatusBody(t, s, c.ID)

	// Both C and D settled to succeeded with NO run_id in this single read.
	byRef := map[string]campaignItemResponse{}
	for _, it := range st.Items {
		byRef[it.IssueRef] = it
	}
	for _, ref := range []string{"issue:100", "issue:101"} {
		it, ok := byRef[ref]
		if !ok {
			t.Fatalf("item %s missing from status", ref)
		}
		if it.State != string(campaign.ItemStateSucceeded) {
			t.Errorf("item %s state = %q, want succeeded (chain converged in one read)", ref, it.State)
		}
		if it.RunID != nil {
			t.Errorf("item %s run_id = %v, want nil (run-less settle)", ref, it.RunID)
		}
	}

	// One settled audit per chain item, both the run-less variant — and the
	// PAYLOAD must be load-bearing: each item's audit is keyed to ITS issue_ref
	// and carries settled_via=issue_closed, state_reason=completed, and
	// outcome=succeeded, with NO run_id. Asserting only the count=2 would pass
	// even if the impl emitted audits without settled_via or for the wrong refs.
	if n := aud.count("campaign_issue_settled"); n != 2 {
		t.Fatalf("campaign_issue_settled = %d, want 2 (both chain items)", n)
	}
	byRefAudit := settledAuditPayloadsByRef(t, aud)
	for _, ref := range []string{"issue:100", "issue:101"} {
		p, ok := byRefAudit[ref]
		if !ok {
			t.Fatalf("no campaign_issue_settled audit for %s (want one per chain item)", ref)
		}
		if got := p["settled_via"]; got != "issue_closed" {
			t.Errorf("%s settled_via = %v, want issue_closed", ref, got)
		}
		if got := p["state_reason"]; got != "completed" {
			t.Errorf("%s state_reason = %v, want completed", ref, got)
		}
		if got := p["outcome"]; got != string(campaign.ItemStateSucceeded) {
			t.Errorf("%s outcome = %v, want succeeded", ref, got)
		}
		if _, hasRun := p["run_id"]; hasRun {
			t.Errorf("%s audit carries run_id = %v, want absent for a run-less settle", ref, p["run_id"])
		}
	}

	// Eligible-slice-never-CLOSED invariant: neither closed item is eligible, and
	// next_action never suggests starting a run on a closed item.
	for _, ref := range []string{"issue:100", "issue:101"} {
		if containsRef(st.Rollup.Eligible, ref) {
			t.Errorf("rollup.Eligible = %v, contains CLOSED item %s", st.Rollup.Eligible, ref)
		}
	}
	if st.NextAction.Action == "start_run" &&
		(st.NextAction.IssueRef == "issue:100" || st.NextAction.IssueRef == "issue:101") {
		t.Errorf("next_action = %+v, must not start_run a CLOSED item", st.NextAction)
	}

	// Whole chain settled → campaign advances to succeeded.
	if st.Campaign.State != string(campaign.StateSucceeded) {
		t.Errorf("campaign state = %q, want succeeded once all items settle", st.Campaign.State)
	}

	// The two-phase design resolves each item's closed-status exactly once — the
	// fixpoint is in-memory, so no O(n^2) GitHub calls.
	if ghState.issueCalls != 2 {
		t.Errorf("issueCalls = %d, want 2 (each item read once; fixpoint is in-memory)", ghState.issueCalls)
	}
	if ghState.installCalls != 1 {
		t.Errorf("installCalls = %d, want 1 (installation resolved once and cached)", ghState.installCalls)
	}
}

// TestReconcileOnRead_RunlessClosedDepOnOpen_StaysBlocked is the negative guard
// preserving DAG order under the #1758 fixpoint: a run-less child C (issue:101)
// that is itself closed-as-completed but whose dependency D (issue:100) is still
// OPEN must NOT be settled. D never enters the done-set, so C's dep is
// unsatisfied and the Phase-2 fixpoint never reaches C — it stays Blocked, never
// Eligible, and emits no settle audit.
func TestReconcileOnRead_RunlessClosedDepOnOpen_StaysBlocked(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "open", stateReason: ""},            // D still open
		101: {state: "closed", stateReason: "completed"}, // C closed out of dep order
	}}
	gh := newRunlessGitHubClient(t, ghState)
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: gh})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
		cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)

	// C is closed but its dep is open → left unsettled, no settle audit.
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (closed item's dep still open)", n)
	}
	for _, it := range st.Items {
		if it.IssueRef == "issue:101" && it.State == string(campaign.ItemStateSucceeded) {
			t.Errorf("item issue:101 settled to succeeded, want unsettled (dep D open)")
		}
	}

	// DAG order preserved: C appears Blocked, never Eligible.
	if !containsRef(st.Rollup.Blocked, "issue:101") {
		t.Errorf("rollup.Blocked = %v, want to contain issue:101 (closed-with-open-dep stays Blocked)", st.Rollup.Blocked)
	}
	if containsRef(st.Rollup.Eligible, "issue:101") {
		t.Errorf("rollup.Eligible = %v, must NOT contain issue:101 (dep unsatisfied)", st.Rollup.Eligible)
	}
}

// TestReconcileOnRead_RunlessIssueOpen_NotSettled: an OPEN issue is not a
// completion — the item is left unsettled and no settle audit is emitted.
func TestReconcileOnRead_RunlessIssueOpen_NotSettled(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "open", stateReason: ""},
	}}
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (open issue not settled)", st.Rollup.Done)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (open issue)", n)
	}
	if ghState.issueCalls != 1 {
		t.Errorf("issueCalls = %d, want 1 (issue was read then rejected)", ghState.issueCalls)
	}
}

// TestReconcileOnRead_RunlessIssueNotPlanned_NotSettled: a closed-as-not_planned
// issue is an abandonment, not a completion — the item is left unsettled.
func TestReconcileOnRead_RunlessIssueNotPlanned_NotSettled(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "not_planned"},
	}}
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (not_planned not settled)", st.Rollup.Done)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (not_planned closure)", n)
	}
}

// TestReconcileOnRead_RunlessNilGitHub_PassSkipped: with no GitHub client the
// run-less pass short-circuits — the item stays pending and the read still 200s.
func TestReconcileOnRead_RunlessNilGitHub_PassSkipped(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	s := New(Config{CampaignRepo: crepo, AuditRepo: aud}) // GitHub + RunRepo nil
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (pass skipped, no GitHub)", st.Rollup.Done)
	}
	if it := crepo.itemsByCmp[c.ID][0]; it.State != campaign.ItemStatePending {
		t.Errorf("item state = %q, want pending (pass skipped)", it.State)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (GitHub unwired)", n)
	}
}

// TestReconcileOnRead_RunlessGetIssueError_StillReturns200: a GetIssue HTTP 500
// is logged-and-swallowed — the item is left pending and the read still 200s
// (best-effort, never fails the read).
func TestReconcileOnRead_RunlessGetIssueError_StillReturns200(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issuesStatus: http.StatusInternalServerError}
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (GetIssue error swallowed)", st.Rollup.Done)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (GetIssue error)", n)
	}
	if ghState.issueCalls != 1 {
		t.Errorf("issueCalls = %d, want 1 (issue read attempted)", ghState.issueCalls)
	}
}

// TestReconcileOnRead_RunlessBadRepo_PassSkipped: a campaign whose repo is not
// in owner/name form is skipped before any GitHub call — the item is left
// unsettled and the read still 200s.
func TestReconcileOnRead_RunlessBadRepo_PassSkipped(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"},
	}}
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	// A repo string with no "/" fails splitRepoFullName.
	c := crepo.seedCampaignWithItems("not-a-valid-repo", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (bad repo, pass skipped)", st.Rollup.Done)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (bad repo)", n)
	}
	if ghState.issueCalls != 0 || ghState.installCalls != 0 {
		t.Errorf("GitHub calls = issue:%d install:%d, want 0/0 (skipped before any call)", ghState.issueCalls, ghState.installCalls)
	}
}

// TestReconcileOnRead_RunlessInstallResolveError_StillReturns200: an
// installation-resolution failure is logged-and-swallowed — the item is left
// pending and the read still 200s (no GetIssue is attempted).
func TestReconcileOnRead_RunlessInstallResolveError_StillReturns200(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installStatus: http.StatusInternalServerError, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"},
	}}
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("issue:100", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID) // asserts 200 internally
	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (install-resolve error swallowed)", st.Rollup.Done)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (install-resolve error)", n)
	}
	if ghState.installCalls != 1 || ghState.issueCalls != 0 {
		t.Errorf("GitHub calls = install:%d issue:%d, want install:1 issue:0 (no GetIssue after resolve failure)", ghState.installCalls, ghState.issueCalls)
	}
}

// TestReconcileOnRead_RunlessNonIssueRef_Skipped: an item whose ref is not
// issue:N (e.g. a Jira key) has no GitHub issue to read — it is skipped BEFORE
// any GitHub call (no installation resolve, no GetIssue).
func TestReconcileOnRead_RunlessNonIssueRef_Skipped(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	ghState := &runlessGitHub{installID: 4242}
	s := New(Config{CampaignRepo: crepo, RunRepo: newFakeRepo(), AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItem("jira:ABC-1", nil, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)
	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (non issue:N ref skipped)", st.Rollup.Done)
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (non-issue ref)", n)
	}
	if ghState.issueCalls != 0 || ghState.installCalls != 0 {
		t.Errorf("GitHub calls = issue:%d install:%d, want 0/0 (skipped before any call)", ghState.issueCalls, ghState.installCalls)
	}
}

// seedTerminalRun inserts a run in the given terminal state into rrepo and
// returns its id, so a class-B (out-of-band terminal) item can carry a faithful
// run link. The settle path never reads the run (the item's terminal state
// already reflects it), but the link is real so the retained-provenance
// assertion is meaningful.
func seedTerminalRun(t *testing.T, rrepo *fakeRepo, state run.State) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	rrepo.mu.Lock()
	rrepo.runs[id] = &run.Run{
		ID:            id,
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerCLI,
		State:         state,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	rrepo.mu.Unlock()
	return id
}

// cItemWithRun builds a campaign item fixture linked to runID (a class-B
// out-of-band-terminal item carries a run link even in its terminal state).
func cItemWithRun(ref string, deps []string, state campaign.ItemState, runID uuid.UUID) *campaign.Item {
	it := cItem(ref, deps, state)
	it.RunID = &runID
	return it
}

// TestReconcileOnRead_OutOfBandTerminal_CancelledDelivered_SettlesAndUnblocks_E2E
// is the #2029 headline cross-boundary path (class B): a CANCELLED item whose
// linked run went terminal-non-succeeded but whose GitHub issue is now
// closed-as-completed (delivered out-of-band) settles succeeded on a status read
// via SettleCampaignItemOutOfBand, its dependent unblocks in the SAME response,
// and the campaign_issue_settled audit carries settled_via=issue_closed AND the
// retained run_id (the field that distinguishes this arm from the run-less arm).
// Directly pins the binding condition's user-visible regression: after the
// settle, next_action targets the DEPENDENT, never a start_run/restart of the
// closed, delivered item.
func TestReconcileOnRead_OutOfBandTerminal_CancelledDelivered_SettlesAndUnblocks_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	rrepo := newFakeRepo()
	runID := seedTerminalRun(t, rrepo, run.StateCancelled)
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"}, // delivered out-of-band
	}}
	gh := newRunlessGitHubClient(t, ghState)
	s := New(Config{CampaignRepo: crepo, RunRepo: rrepo, AuditRepo: aud, GitHub: gh})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItemWithRun("issue:100", nil, campaign.ItemStateCancelled, runID),
		cItem("issue:101", []string{"issue:100"}, campaign.ItemStatePending),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)

	// issue:100 settled succeeded (Done), run link RETAINED (provenance).
	byRef := map[string]campaignItemResponse{}
	for _, it := range st.Items {
		byRef[it.IssueRef] = it
	}
	if got := byRef["issue:100"]; got.State != string(campaign.ItemStateSucceeded) {
		t.Errorf("item issue:100 state = %q, want succeeded (out-of-band settle)", got.State)
	}
	if got := byRef["issue:100"]; got.RunID == nil || *got.RunID != runID {
		t.Errorf("item issue:100 run_id = %v, want %s retained (provenance)", got.RunID, runID)
	}
	if len(st.Rollup.Done) != 1 || st.Rollup.Done[0] != "issue:100" {
		t.Errorf("rollup.Done = %v, want [issue:100] after out-of-band settle", st.Rollup.Done)
	}
	// The wedge cleared: the settled item is no longer offered for restart, and
	// its dependent unblocked.
	if containsRef(st.Rollup.Cancelled, "issue:100") {
		t.Errorf("rollup.Cancelled = %v, must NOT contain the settled issue:100", st.Rollup.Cancelled)
	}
	if len(st.Rollup.Eligible) != 1 || st.Rollup.Eligible[0] != "issue:101" {
		t.Errorf("rollup.Eligible = %v, want [issue:101] (dependent unblocked)", st.Rollup.Eligible)
	}
	// Binding condition: next_action targets the DEPENDENT, never a
	// start_run/restart of the closed, delivered issue:100.
	if st.NextAction.Action != "start_run" || st.NextAction.IssueRef != "issue:101" {
		t.Errorf("next_action = %+v, want start_run issue:101 (dependent)", st.NextAction)
	}
	if st.NextAction.IssueRef == "issue:100" {
		t.Errorf("next_action targets the settled issue:100 = %+v, want it NEVER offered for start_run/restart", st.NextAction)
	}

	// Exactly one settle audit, class B: settled_via=issue_closed AND run_id present.
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Fatalf("campaign_issue_settled = %d, want 1", n)
	}
	p := settledAuditPayload(t, aud)
	if p["settled_via"] != "issue_closed" {
		t.Errorf("settled_via = %v, want issue_closed", p["settled_via"])
	}
	if p["state_reason"] != "completed" {
		t.Errorf("state_reason = %v, want completed", p["state_reason"])
	}
	if p["outcome"] != "succeeded" {
		t.Errorf("outcome = %v, want succeeded", p["outcome"])
	}
	if got, ok := p["run_id"].(string); !ok || got != runID.String() {
		t.Errorf("payload run_id = %v (ok=%v), want %s present (out-of-band-terminal marker)", p["run_id"], ok, runID)
	}
	// The class-B settle went through the guard-bypassing repo method exactly once.
	if crepo.settleOOBCalls != 1 {
		t.Errorf("settleOOBCalls = %d, want 1 (class-B bypass method)", crepo.settleOOBCalls)
	}
}

// TestReconcileOnRead_OutOfBandTerminal_SingleItem_NextActionComplete_E2E is the
// purest form of the binding condition (#2029): a SINGLE delivered-out-of-band
// cancelled item, no dependents. BEFORE the fix the campaign wedges — the item
// stays cancelled, its rollup keeps it Restartable, and next_action wrongly
// advises start_run (restart) on the closed issue. AFTER the fix the status read
// settles it succeeded and next_action for the settled item is COMPLETE — the
// item is no longer offered start_run and no restart is advised.
func TestReconcileOnRead_OutOfBandTerminal_SingleItem_NextActionComplete_E2E(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	rrepo := newFakeRepo()
	runID := seedTerminalRun(t, rrepo, run.StateCancelled)
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"},
	}}
	s := New(Config{CampaignRepo: crepo, RunRepo: rrepo, AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItemWithRun("issue:100", nil, campaign.ItemStateCancelled, runID),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)

	// The settled item: succeeded, in Done, NOT offered for restart.
	if len(st.Rollup.Done) != 1 || st.Rollup.Done[0] != "issue:100" {
		t.Errorf("rollup.Done = %v, want [issue:100]", st.Rollup.Done)
	}
	if containsRef(st.Rollup.Cancelled, "issue:100") {
		t.Errorf("rollup.Cancelled = %v, must NOT contain the settled item", st.Rollup.Cancelled)
	}
	// Binding condition, pinned directly: next_action is complete (not
	// start_run/restart of the closed issue).
	if st.NextAction.Action != "complete" {
		t.Errorf("next_action = %+v, want complete (settled item never restarted)", st.NextAction)
	}
	if st.NextAction.Action == "start_run" {
		t.Errorf("next_action = %+v, must NOT advise start_run/restart of the delivered issue", st.NextAction)
	}
	// Whole campaign settled → succeeded.
	if st.Campaign.State != string(campaign.StateSucceeded) {
		t.Errorf("campaign state = %q, want succeeded once the sole item settles", st.Campaign.State)
	}
	if crepo.settleOOBCalls != 1 {
		t.Errorf("settleOOBCalls = %d, want 1", crepo.settleOOBCalls)
	}
}

// TestReconcileOnRead_OutOfBandTerminal_NotPlanned_NotSettled is the class-B
// not_planned guard: a cancelled, run-linked item whose issue is closed as
// NOT_PLANNED is an abandonment, not a delivery — it must stay cancelled, emit no
// settle audit, and never invoke the bypass method (the closed-as-completed-only
// guard is unchanged for class B).
func TestReconcileOnRead_OutOfBandTerminal_NotPlanned_NotSettled(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	rrepo := newFakeRepo()
	runID := seedTerminalRun(t, rrepo, run.StateCancelled)
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "not_planned"},
	}}
	s := New(Config{CampaignRepo: crepo, RunRepo: rrepo, AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItemWithRun("issue:100", nil, campaign.ItemStateCancelled, runID),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)

	if len(st.Rollup.Done) != 0 {
		t.Errorf("rollup.Done = %v, want empty (not_planned not settled)", st.Rollup.Done)
	}
	for _, it := range st.Items {
		if it.IssueRef == "issue:100" && it.State != string(campaign.ItemStateCancelled) {
			t.Errorf("item issue:100 state = %q, want cancelled (not_planned left unsettled)", it.State)
		}
	}
	if n := aud.count("campaign_issue_settled"); n != 0 {
		t.Errorf("campaign_issue_settled = %d, want 0 (not_planned closure)", n)
	}
	if crepo.settleOOBCalls != 0 {
		t.Errorf("settleOOBCalls = %d, want 0 (bypass method never invoked for not_planned)", crepo.settleOOBCalls)
	}
}

// TestReconcileOnRead_OutOfBandTerminal_FailedDelivered_Settles is the class-B
// failed-run variant: a FAILED (not cancelled) item whose issue is
// closed-as-completed settles succeeded the same way — the bypass admits both
// terminal-non-succeeded from-states, with run_id retained.
func TestReconcileOnRead_OutOfBandTerminal_FailedDelivered_Settles(t *testing.T) {
	crepo := newFakeCampaignRepo()
	aud := &campaignAuditRecorder{}
	rrepo := newFakeRepo()
	runID := seedTerminalRun(t, rrepo, run.StateFailed)
	ghState := &runlessGitHub{installID: 4242, issues: map[int]runlessIssue{
		100: {state: "closed", stateReason: "completed"},
	}}
	s := New(Config{CampaignRepo: crepo, RunRepo: rrepo, AuditRepo: aud, GitHub: newRunlessGitHubClient(t, ghState)})
	c := crepo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		cItemWithRun("issue:100", nil, campaign.ItemStateFailed, runID),
	})
	c.State = campaign.StateRunning

	st := getCampaignStatusBody(t, s, c.ID)

	byRef := map[string]campaignItemResponse{}
	for _, it := range st.Items {
		byRef[it.IssueRef] = it
	}
	if got := byRef["issue:100"]; got.State != string(campaign.ItemStateSucceeded) {
		t.Errorf("item issue:100 state = %q, want succeeded (failed→succeeded out-of-band)", got.State)
	}
	if got := byRef["issue:100"]; got.RunID == nil || *got.RunID != runID {
		t.Errorf("item issue:100 run_id = %v, want %s retained", got.RunID, runID)
	}
	if n := aud.count("campaign_issue_settled"); n != 1 {
		t.Fatalf("campaign_issue_settled = %d, want 1", n)
	}
	p := settledAuditPayload(t, aud)
	if p["settled_via"] != "issue_closed" {
		t.Errorf("settled_via = %v, want issue_closed", p["settled_via"])
	}
	if got, ok := p["run_id"].(string); !ok || got != runID.String() {
		t.Errorf("payload run_id = %v (ok=%v), want %s present", p["run_id"], ok, runID)
	}
	if crepo.settleOOBCalls != 1 {
		t.Errorf("settleOOBCalls = %d, want 1", crepo.settleOOBCalls)
	}
}

// TestCampaignDecomposedParent_ChildrenBindOwnSlice_E2E is the #1721 cross-
// boundary regression pinning THIS run's shape. It spans two files: run creation
// in runs.go (StartRunForCampaignIssue mints a campaign parent whose IssueContext
// is nil — the GitHub fake serves the install + spec but not the issue, so
// hydration degrades to nil) and the prompt-serving handler in prompt.go (each
// decomposed child's implement prompt narrows to its OWN slice by the persisted
// SliceIndex). Before the fix, a nil-IssueContext parent broke the sub-plan title
// match and every child silently inherited the parent's full scope (reopening
// #1669); the assertion is that no child's served scope_files equal the parent
// union.
//
// The fan-out children are inserted directly (the in-memory promptRunRepo cannot
// run the real orchestrator, which the fanout_test.go SliceIndex pin covers) with
// the exact fields orchestrator.fanoutIfDecomposed persists: DecomposedFrom +
// ParentRunID + a distinct SliceIndex, and a nil IssueContext.
func TestCampaignDecomposedParent_ChildrenBindOwnSlice_E2E(t *testing.T) {
	rr := newPromptRunRepo()
	art := newFakeArtifactRepo()
	sf := newSigningFake()
	// GitHub fake serves installation + workflow spec (so StartRunForCampaignIssue
	// reaches CreateRunForTrigger) but NOT the issue endpoint — so the #1721
	// hydration degrades to a nil IssueContext, the campaign-minted regression
	// shape.
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	ghSrv := fake.server(t)
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, ArtifactRepo: art, SigningRepo: sf, GitHub: gh})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	// runs.go boundary: mint the campaign parent run.
	parent, err := s.StartRunForCampaignIssue(context.Background(),
		"kuhlman-labs/fishhawk", "issue:100", "feature_change", "", "local")
	if err != nil {
		t.Fatalf("StartRunForCampaignIssue: %v", err)
	}
	if parent.IssueContext != nil {
		t.Fatalf("parent.IssueContext = %+v, want nil (campaign-minted regression shape)", parent.IssueContext)
	}

	// Locate the parent's plan stage (seeded from gatedSpecYAML's feature_change).
	var planStageID uuid.UUID
	for _, st := range rr.stagesByRunID[parent.ID] {
		if st.Type == run.StageTypePlan {
			planStageID = st.ID
		}
	}
	if planStageID == uuid.Nil {
		t.Fatalf("parent has no plan stage; stages=%+v", rr.stagesByRunID[parent.ID])
	}

	// Seed the approved decomposed plan: full union of three disjoint slices.
	parentPlan := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "parent plan",
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "pkg/a/a.go", Operation: plan.FileOpModify},
			{Path: "pkg/b/b.go", Operation: plan.FileOpModify},
			{Path: "pkg/c/c.go", Operation: plan.FileOpModify},
		}},
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Decomposition: &plan.Decomposition{
			Rationale: "scope split",
			SubPlans: []plan.SubPlanSummary{
				{Title: "Part A", ScopeHint: "A", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
				{Title: "Part B", ScopeHint: "B", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/b/b.go", Operation: plan.FileOpModify}}}},
				{Title: "Part C", ScopeHint: "C", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/c/c.go", Operation: plan.FileOpModify}}}},
			},
		},
	}
	planBytes, err := json.Marshal(parentPlan)
	if err != nil {
		t.Fatalf("marshal parent plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID:       planStageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &sv,
		Content:       planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	parentUnion := map[string]bool{"pkg/a/a.go": true, "pkg/b/b.go": true, "pkg/c/c.go": true}
	wantSlice := []string{"pkg/a/a.go", "pkg/b/b.go", "pkg/c/c.go"}

	// Simulate the fan-out: mint one implement-only child per slice with the exact
	// linkage fields the orchestrator persists — DecomposedFrom + ParentRunID + a
	// distinct SliceIndex, nil IssueContext.
	for i := range parentPlan.Decomposition.SubPlans {
		childID := uuid.New()
		implID := uuid.New()
		idx := i
		parentID := parent.ID
		rr.getRuns[childID] = &run.Run{
			ID:             childID,
			Repo:           parent.Repo,
			WorkflowID:     "feature_change",
			TriggerSource:  run.TriggerGitHubIssue,
			ParentRunID:    &parentID,
			DecomposedFrom: &parentID,
			SliceIndex:     &idx,
			IssueContext:   nil, // inherited from the nil-IssueContext parent
		}
		implStage := &run.Stage{ID: implID, RunID: childID, Type: run.StageTypeImplement, State: run.StageStatePending}
		rr.stagesByRunID[childID] = []*run.Stage{implStage}
		rr.getStages[implID] = implStage

		// prompt.go boundary: fetch the child's implement prompt.
		priv, _ := sf.issue(t, childID)
		w := promptRequest(t, s, childID, implID, priv, "")
		if w.Code != http.StatusOK {
			t.Fatalf("slice %d: prompt status = %d, want 200:\n%s", i, w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("slice %d: decode: %v", i, err)
		}
		got := make([]string, 0, len(resp.ScopeFiles))
		for _, f := range resp.ScopeFiles {
			got = append(got, f.Path)
		}
		// The child owns exactly its one slice file plus the coupled _test.go.
		want := []string{wantSlice[i], strings.TrimSuffix(wantSlice[i], ".go") + "_test.go"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("slice %d: scope_files = %v, want its own slice %v", i, got, want)
		}
		// And crucially: the served scope is NEVER the parent's full union.
		if len(got) == len(parentUnion) {
			allInUnion := true
			for _, p := range got {
				if !parentUnion[p] {
					allInUnion = false
					break
				}
			}
			if allInUnion {
				t.Errorf("slice %d: child served the parent's full union %v — the #1721 regression", i, got)
			}
		}
	}
}
