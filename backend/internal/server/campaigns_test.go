package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
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
			// FAILED still wins over a PAUSED item (failed is the strict-first check).
			name:    "failed and paused both present -> attention",
			elig:    campaign.Eligibility{Failed: []string{"issue:5"}, Paused: []string{"issue:6"}},
			want:    "attention",
			wantRef: "issue:5",
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
