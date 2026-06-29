package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
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
	createErr error
	getErr    error
	listErr   error
	itemsErr  error
}

func newFakeCampaignRepo() *fakeCampaignRepo {
	return &fakeCampaignRepo{
		campaigns:  map[uuid.UUID]*campaign.Campaign{},
		itemsByCmp: map[uuid.UUID][]*campaign.Item{},
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
		ID:        uuid.New(),
		Repo:      p.Repo,
		EpicRef:   p.EpicRef,
		State:     campaign.StatePending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	f.campaigns[c.ID] = c
	return c, nil
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
	if f.itemsErr != nil {
		return nil, f.itemsErr
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
	if fp.captured.Target.InstallationID != 4242 {
		t.Errorf("provider Target.InstallationID = %d, want 4242", fp.captured.Target.InstallationID)
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

// TestCreateCampaign_NoDedup_DistinctIDs is the IDEMPOTENCY-HONESTY pin: two
// POSTs of the same {repo, epic_ref} WITH an Idempotency-Key header set must
// BOTH return 201 with DISTINCT campaign ids (the header is ignored, not
// honoured). No dedup is implementable — migration 0039 has no
// idempotency_key column and campaign.Repository has no dedup lookup.
func TestCreateCampaign_NoDedup_DistinctIDs(t *testing.T) {
	fp := &fakeEpicProvider{result: smallDAG()}
	registerEpicProvider(t, fp)
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: install resolution skipped

	post := func() campaignResponse {
		req := httptest.NewRequest(http.MethodPost, "/v0/campaigns",
			strings.NewReader(`{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`))
		req.Header.Set("Idempotency-Key", "the-same-key")
		w := httptest.NewRecorder()
		s.handleCreateCampaign(w, withAuth(req))
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
		}
		var c campaignResponse
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return c
	}

	first := post()
	second := post()
	if first.ID == second.ID {
		t.Errorf("both POSTs returned the same id %s; the Idempotency-Key header must NOT dedup", first.ID)
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
		name    string
		elig    campaign.Eligibility
		want    string
		wantRef string
	}{
		{
			// THE mandated test: FAILED wins over an existing ELIGIBLE item.
			name:    "failed and eligible both present -> attention",
			elig:    campaign.Eligibility{Failed: []string{"issue:5"}, Eligible: []string{"issue:6"}},
			want:    "attention",
			wantRef: "issue:5",
		},
		{
			name:    "eligible only -> start_run",
			elig:    campaign.Eligibility{Eligible: []string{"issue:6"}},
			want:    "start_run",
			wantRef: "issue:6",
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
			name:    "failed only -> attention",
			elig:    campaign.Eligibility{Failed: []string{"issue:10"}},
			want:    "attention",
			wantRef: "issue:10",
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
		})
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

func TestGetCampaignStatus_RollupAndNextAction(t *testing.T) {
	repo := newFakeCampaignRepo()
	// A failed item alongside an eligible item: next_action must be attention.
	c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
		{IssueRef: "issue:100", State: campaign.ItemStateFailed},
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
	// FAILED wins over the eligible item.
	if status.NextAction.Action != "attention" || status.NextAction.IssueRef != "issue:100" {
		t.Errorf("next_action = %+v, want attention issue:100", status.NextAction)
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
