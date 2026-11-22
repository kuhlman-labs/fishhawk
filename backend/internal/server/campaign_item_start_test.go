package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- Harness (#5266 slice 1) ---

// hookedSpecGitHub is a GitHub API stub serving the installation, contents
// (workflow spec) and issue routes a campaign item start reaches. On the item
// start path only GetWorkflowSpec inside StartRunForCampaignIssue (after the
// claim) reaches the contents route, so a hook on that route interleaves with
// the mint deterministically.
//
// It is UNARMED by default (U4b): GetWorkflowSpec is also reached by the
// create-time admission screen and the hotfix route, so a test seeds its
// campaign and items through the pg repository and arms only after seeding.
// arm installs a hook that runs ONCE, guarded by an atomic.Bool
// CompareAndSwap rather than sync.Once because the hook may re-enter the
// route; no lock is held while it runs, and armedHits counts only the hits
// made after arming.
type hookedSpecGitHub struct {
	specJSON   string
	specStatus atomic.Int32
	armed      atomic.Bool
	fired      atomic.Bool
	hits       atomic.Int32
	mu         sync.Mutex
	hook       func()
	srv        *httptest.Server
}

func newHookedSpecGitHub(t *testing.T, specYAML string) *hookedSpecGitHub {
	t.Helper()
	g := &hookedSpecGitHub{
		specJSON: `{"path":".fishhawk/workflows.yaml","sha":"spec_sha","content":"` +
			base64.StdEncoding.EncodeToString([]byte(specYAML)) + `","encoding":"base64","type":"file"}`,
	}
	g.specStatus.Store(http.StatusOK)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":12345,"app_id":1}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/", func(w http.ResponseWriter, _ *http.Request) {
		if g.armed.Load() {
			g.hits.Add(1)
			g.mu.Lock()
			hook := g.hook
			g.mu.Unlock()
			if hook != nil && g.fired.CompareAndSwap(false, true) {
				hook()
			}
		}
		status := int(g.specStatus.Load())
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, g.specJSON)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"number":100,"title":"Campaign item","body":"body","state":"open"}`)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// arm installs hook (nil = count only) and starts counting contents hits.
func (g *hookedSpecGitHub) arm(hook func()) {
	g.mu.Lock()
	g.hook = hook
	g.mu.Unlock()
	g.armed.Store(true)
}

func (g *hookedSpecGitHub) armedHits() int { return int(g.hits.Load()) }

func (g *hookedSpecGitHub) client() *githubclient.Client {
	return &githubclient.Client{
		BaseURL: g.srv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
}

// itemStartClock is the fixed, test-advanced server clock. The server's
// nowFunc and (in the race tests) the driver Ticker's Now read the SAME
// instance, so every claim stamp and expiry comparison uses one source.
type itemStartClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *itemStartClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *itemStartClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *itemStartClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// ctxAuditRecorder wraps campaignAuditRecorder: an append on a done context
// returns ctx.Err() (as the real chained append would), and fail makes every
// append error.
type ctxAuditRecorder struct {
	*campaignAuditRecorder
	fail atomic.Bool
}

func (a *ctxAuditRecorder) AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.fail.Load() {
		return nil, errors.New("audit append unavailable")
	}
	return a.campaignAuditRecorder.AppendGlobalChained(ctx, p)
}

// payloads returns the decoded payloads recorded under category.
func (a *ctxAuditRecorder) payloads(t *testing.T, category string) []map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, e := range a.entries {
		if e.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		out = append(out, m)
	}
	return out
}

// faultyCampaignRepo and faultyRunRepo embed the INTERFACE, so each override
// is the only depth-0 method of its name and no promoted-method ambiguity
// arises.
type faultyCampaignRepo struct {
	campaign.Repository
	linkErr       error
	transitionErr error
}

func (f *faultyCampaignRepo) LinkClaimedCampaignItemRun(ctx context.Context, p campaign.LinkClaimedCampaignItemRunParams) (*campaign.Item, error) {
	if f.linkErr != nil {
		return nil, f.linkErr
	}
	return f.Repository.LinkClaimedCampaignItemRun(ctx, p)
}

func (f *faultyCampaignRepo) TransitionCampaignItem(ctx context.Context, id uuid.UUID, to campaign.ItemState) (*campaign.Item, error) {
	if f.transitionErr != nil && to == campaign.ItemStateRunning {
		return nil, f.transitionErr
	}
	return f.Repository.TransitionCampaignItem(ctx, id, to)
}

type faultyRunRepo struct {
	run.Repository
	cancelErr      error
	createStageErr error
	// slowCreateRun commits the run on a DETACHED context, then blocks until
	// the caller's context is done before returning it: a commit that raced
	// the start deadline.
	slowCreateRun bool
}

func (f *faultyRunRepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	if !f.slowCreateRun {
		return f.Repository.CreateRun(ctx, p)
	}
	created, err := f.Repository.CreateRun(context.WithoutCancel(ctx), p)
	if err != nil {
		return nil, err
	}
	<-ctx.Done()
	return created, nil
}

func (f *faultyRunRepo) CreateStage(ctx context.Context, p run.CreateStageParams) (*run.Stage, error) {
	if f.createStageErr != nil {
		return nil, f.createStageErr
	}
	return f.Repository.CreateStage(ctx, p)
}

func (f *faultyRunRepo) TransitionRun(ctx context.Context, id uuid.UUID, to run.State) (*run.Run, error) {
	if f.cancelErr != nil && to == run.StateCancelled {
		return nil, f.cancelErr
	}
	return f.Repository.TransitionRun(ctx, id, to)
}

// itemStartPG is a Server over REAL Postgres campaign and run repos (each
// wrapped in its fault injector), a fixed clock and a ctx-checking audit
// recorder, plus one seeded running campaign with one pending item.
type itemStartPG struct {
	s         *Server
	pool      *pgxpool.Pool
	campaigns campaign.Repository
	runs      run.Repository
	fc        *faultyCampaignRepo
	fr        *faultyRunRepo
	aud       *ctxAuditRecorder
	clock     *itemStartClock
	gh        *hookedSpecGitHub
	c         *campaign.Campaign
	item      *campaign.Item
}

var itemStartT0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// newItemStartServerPG has the newCampaignStartServerPG shape. Seeding goes
// through the pg repository (never POST /v0/campaigns or the hotfix route),
// so gh's hook, armed afterwards, fires only inside the item start.
func newItemStartServerPG(t *testing.T, gh *hookedSpecGitHub) *itemStartPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &itemStartPG{
		pool:      pool,
		campaigns: campaign.NewPostgresRepository(pool),
		runs:      run.NewPostgresRepository(pool),
		aud:       &ctxAuditRecorder{campaignAuditRecorder: &campaignAuditRecorder{}},
		clock:     &itemStartClock{now: itemStartT0},
		gh:        gh,
	}
	f.fc = &faultyCampaignRepo{Repository: f.campaigns}
	f.fr = &faultyRunRepo{Repository: f.runs}
	f.s = New(Config{Addr: "127.0.0.1:0", CampaignRepo: f.fc, RunRepo: f.fr, AuditRepo: f.aud, GitHub: gh.client()})
	f.s.nowFunc = f.clock.Now

	ctx := context.Background()
	c, err := f.campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{
		Repo: "kuhlman-labs/fishhawk", EpicRef: "issue:99", WorkingDir: seededCampaignWorkingDir,
	})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	item, err := f.campaigns.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: "issue:100"})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if c, err = f.campaigns.TransitionCampaign(ctx, c.ID, campaign.StateRunning); err != nil {
		t.Fatalf("campaign->running: %v", err)
	}
	f.c, f.item = c, item
	return f
}

func (f *itemStartPG) request(budgets itemStartBudgets) itemStartRequest {
	return itemStartRequest{
		Campaign: f.c,
		Item:     f.item,
		Params: StartRunForCampaignIssueParams{
			Repo: f.c.Repo, IssueRef: f.item.IssueRef, WorkflowID: "feature_change",
			RunnerKind: "local", WorkingDir: seededCampaignWorkingDir,
		},
		Budgets: budgets,
	}
}

type itemStartResult struct {
	w    *httptest.ResponseRecorder
	run  *run.Run
	item *campaign.Item
	ok   bool
}

// callCore calls s.startClaimedCampaignItemRun through a ResponseRecorder,
// under an operator identity on reqCtx (nil = Background).
func (f *itemStartPG) callCore(t *testing.T, reqCtx context.Context, req itemStartRequest) itemStartResult {
	t.Helper()
	if reqCtx == nil {
		reqCtx = context.Background()
	}
	hr := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+f.c.ID.String()+"/runs", nil).WithContext(reqCtx)
	w := httptest.NewRecorder()
	rn, it, ok := f.s.startClaimedCampaignItemRun(w, withAuth(hr), req)
	return itemStartResult{w: w, run: rn, item: it, ok: ok}
}

func (f *itemStartPG) claimRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM campaign_item_start_claims WHERE item_id = $1`, f.item.ID).Scan(&n); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	return n
}

// runRows returns every run row's id and state.
func (f *itemStartPG) runRows(t *testing.T) map[uuid.UUID]string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT id, state FROM runs`)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("scan run: %v", err)
		}
		out[id] = state
	}
	return out
}

func (f *itemStartPG) readItem(t *testing.T) *campaign.Item {
	t.Helper()
	it, err := f.campaigns.GetCampaignItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("read item: %v", err)
	}
	return it
}

func decodeItemStartError(t *testing.T, w *httptest.ResponseRecorder) (code string, details map[string]any) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return body.Error.Code, body.Error.Details
}

// onlyRun returns the single run row, failing when there is not exactly one.
func (f *itemStartPG) onlyRun(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	rows := f.runRows(t)
	if len(rows) != 1 {
		t.Fatalf("run rows = %v, want exactly one", rows)
	}
	for id, st := range rows {
		return id, st
	}
	panic("unreachable")
}

// assertAbandoned asserts exactly one campaign_item_run_abandoned row for
// runID with reason and cancelled.
func (f *itemStartPG) assertAbandoned(t *testing.T, runID uuid.UUID, reason string, cancelled bool) map[string]any {
	t.Helper()
	ps := f.aud.payloads(t, categoryCampaignItemRunAbandoned)
	if len(ps) != 1 {
		t.Fatalf("campaign_item_run_abandoned rows = %d (%v), want 1", len(ps), ps)
	}
	p := ps[0]
	if p["run_id"] != runID.String() || p["reason"] != reason || p["cancelled"] != cancelled || p["starter"] != campaign.StarterOperator {
		t.Errorf("abandon payload = %v, want run_id %s reason %s cancelled %v starter operator", p, runID, reason, cancelled)
	}
	return p
}

// --- Core tests ---

func TestStartClaimedCampaignItemRun_Success_ClaimsMintsLinksAndConsumesClaim(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.gh.arm(nil)
	var tailCtxErr error
	tailRan := false
	req := f.request(itemStartBudgets{})
	req.OnStarted = func(ctx context.Context, _ *run.Run, _ *campaign.Item) {
		tailRan = true
		tailCtxErr = ctx.Err()
	}
	res := f.callCore(t, nil, req)
	if !res.ok {
		t.Fatalf("core ok = false, body %s", res.w.Body.String())
	}
	if res.item.State != campaign.ItemStateRunning {
		t.Errorf("returned item state = %q, want running", res.item.State)
	}
	got := f.readItem(t)
	if got.RunID == nil || *got.RunID != res.run.ID || got.State != campaign.ItemStateRunning {
		t.Errorf("persisted item run_id=%v state=%q, want %s running", got.RunID, got.State, res.run.ID)
	}
	if n := f.claimRows(t); n != 0 {
		t.Errorf("claim rows = %d after a successful link, want 0 (consumed)", n)
	}
	if id, _ := f.onlyRun(t); id != res.run.ID {
		t.Errorf("run row %s, want %s", id, res.run.ID)
	}
	if f.gh.armedHits() != 1 {
		t.Errorf("spec route hits = %d, want 1", f.gh.armedHits())
	}
	if !tailRan || tailCtxErr != nil {
		t.Errorf("OnStarted ran=%v ctxErr=%v, want it run on a live start context", tailRan, tailCtxErr)
	}
	if n := f.aud.count(categoryCampaignItemRunAbandoned); n != 0 {
		t.Errorf("abandoned rows = %d, want 0", n)
	}
}

func TestStartClaimedCampaignItemRun_LiveClaimHeld_409StartInProgress_MintsNothing(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	held, err := f.campaigns.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
		ItemID: f.item.ID, Starter: campaign.StarterCampaignDriver, Clock: func() time.Time { return itemStartT0.Add(-time.Minute) },
	})
	if err != nil {
		t.Fatalf("seed driver claim: %v", err)
	}
	f.gh.arm(nil)
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.ok || res.w.Code != http.StatusConflict {
		t.Fatalf("status = %d ok=%v, want 409; body %s", res.w.Code, res.ok, res.w.Body.String())
	}
	code, d := decodeItemStartError(t, res.w)
	if code != "item_not_eligible" || d["start_in_progress"] != true || d["claim_starter"] != campaign.StarterCampaignDriver {
		t.Errorf("code=%q details=%v, want item_not_eligible start_in_progress claim_starter campaign_driver", code, d)
	}
	if d["claim_expires_at"] != held.ExpiresAt.UTC().Format(time.RFC3339Nano) {
		t.Errorf("claim_expires_at = %v, want %s", d["claim_expires_at"], held.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	if rows := f.runRows(t); len(rows) != 0 {
		t.Errorf("run rows = %v, want none minted", rows)
	}
	if f.gh.armedHits() != 0 {
		t.Errorf("spec route hits = %d, want 0 (no mint attempted)", f.gh.armedHits())
	}
	if f.claimRows(t) != 1 {
		t.Error("the driver's live claim was disturbed")
	}
}

func TestStartClaimedCampaignItemRun_ExpiredClaim_Reclaimed_Starts(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	if _, err := f.campaigns.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
		ItemID: f.item.ID, Starter: campaign.StarterCampaignDriver, Clock: func() time.Time { return itemStartT0.Add(-campaign.ItemStartClaimTTL) },
	}); err != nil {
		t.Fatalf("seed crashed starter's claim: %v", err)
	}
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if !res.ok {
		t.Fatalf("core ok = false, body %s", res.w.Body.String())
	}
	if got := f.readItem(t); got.RunID == nil || *got.RunID != res.run.ID {
		t.Errorf("item run_id = %v, want %s", got.RunID, res.run.ID)
	}
	if f.claimRows(t) != 0 {
		t.Error("claim row left after the reclaimed start linked")
	}
}

func TestStartClaimedCampaignItemRun_LinkedRace_409NamesWinner(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	ref := "issue:100"
	winner, err := f.runs.CreateRun(context.Background(), run.CreateRunParams{Repo: f.c.Repo, WorkflowID: "feature_change", WorkflowSHA: "w", TriggerSource: run.TriggerGitHubIssue, TriggerRef: &ref})
	if err != nil {
		t.Fatalf("create winner: %v", err)
	}
	if _, err := f.campaigns.SetCampaignItemRun(context.Background(), f.item.ID, &winner.ID); err != nil {
		t.Fatalf("link winner: %v", err)
	}
	f.gh.arm(nil)
	// f.item is the UNLINKED snapshot the handler read before the race.
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", res.w.Code, res.w.Body.String())
	}
	code, d := decodeItemStartError(t, res.w)
	if code != "item_not_eligible" || d["run_id"] != winner.ID.String() {
		t.Errorf("code=%q details=%v, want item_not_eligible run_id %s", code, d, winner.ID)
	}
	if _, ok := d["start_in_progress"]; ok {
		t.Error("a linked refusal must not say start_in_progress")
	}
	if rows := f.runRows(t); len(rows) != 1 {
		t.Errorf("run rows = %v, want only the winner", rows)
	}
	if f.gh.armedHits() != 0 || f.claimRows(t) != 0 {
		t.Errorf("spec hits=%d claims=%d, want 0/0", f.gh.armedHits(), f.claimRows(t))
	}
}

func TestStartClaimedCampaignItemRun_PreMintFailure_502AndReleasesClaim(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.gh.specStatus.Store(http.StatusInternalServerError)
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", res.w.Code, res.w.Body.String())
	}
	if code, _ := decodeItemStartError(t, res.w); code != "campaign_run_start_failed" {
		t.Errorf("code = %q, want campaign_run_start_failed", code)
	}
	if rows := f.runRows(t); len(rows) != 0 {
		t.Errorf("run rows = %v, want none", rows)
	}
	// Same clock instant: only a released claim lets the retry through.
	f.gh.specStatus.Store(http.StatusOK)
	res = f.callCore(t, nil, f.request(itemStartBudgets{}))
	if !res.ok {
		t.Fatalf("retry at the same instant: status %d ok=%v body %s, want success", res.w.Code, res.ok, res.w.Body.String())
	}
}

func TestStartClaimedCampaignItemRun_PostMintStageFailure_500IncompleteCancelsRun(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.fr.createStageErr = errors.New("stage insert failed")
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", res.w.Code, res.w.Body.String())
	}
	code, d := decodeItemStartError(t, res.w)
	runID, state := f.onlyRun(t)
	if code != "campaign_run_start_incomplete" || d["run_id"] != runID.String() || d[itemStartOutcomeCancelled] != runID.String() {
		t.Errorf("code=%q details=%v, want campaign_run_start_incomplete run_id+cancelled_run_id %s", code, d, runID)
	}
	if _, ok := d[itemStartOutcomeCancelFailed]; ok {
		t.Error("both outcome keys present")
	}
	if state != string(run.StateCancelled) {
		t.Errorf("minted run state = %q, want cancelled", state)
	}
	if got := f.readItem(t); got.RunID != nil {
		t.Errorf("item linked to %v, want unlinked", *got.RunID)
	}
	if f.claimRows(t) != 0 {
		t.Error("claim not released after the incomplete start")
	}
	f.assertAbandoned(t, runID, itemRunAbandonStartIncomplete, true)
}

// reclaimAsDriverDuringMint arms the hook to reclaim the item as the driver at
// clock+TTL, inside the operator's mint, so the operator's claimed link finds
// its claim replaced.
func (f *itemStartPG) reclaimAsDriverDuringMint(t *testing.T) {
	f.gh.arm(func() {
		f.clock.advance(campaign.ItemStartClaimTTL)
		if _, err := f.campaigns.ClaimCampaignItemStart(context.Background(), campaign.ClaimCampaignItemStartParams{
			ItemID: f.item.ID, Starter: campaign.StarterCampaignDriver, Clock: f.clock.Now,
		}); err != nil {
			t.Errorf("hook: driver reclaim: %v", err)
		}
	})
}

func TestStartClaimedCampaignItemRun_ClaimLostAfterMint_CancelsRunAndReports(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.reclaimAsDriverDuringMint(t)
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", res.w.Code, res.w.Body.String())
	}
	code, d := decodeItemStartError(t, res.w)
	runID, state := f.onlyRun(t)
	if code != "item_not_eligible" || d["claim_lost"] != true || d[itemStartOutcomeCancelled] != runID.String() {
		t.Errorf("code=%q details=%v, want item_not_eligible claim_lost cancelled_run_id %s", code, d, runID)
	}
	if state != string(run.StateCancelled) {
		t.Errorf("minted run state = %q, want cancelled", state)
	}
	if got := f.readItem(t); got.RunID != nil {
		t.Errorf("item linked to %v, want unlinked (the driver owns it now)", *got.RunID)
	}
	if f.claimRows(t) != 1 {
		t.Error("the driver's replacing claim was disturbed by the loser's release")
	}
	f.assertAbandoned(t, runID, itemRunAbandonClaimConflict, true)
}

func TestStartClaimedCampaignItemRun_ClaimLostAfterMint_CancelFails_ReportsCancelFailed(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.fr.cancelErr = errors.New("cancel transition failed")
	f.reclaimAsDriverDuringMint(t)
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", res.w.Code, res.w.Body.String())
	}
	_, d := decodeItemStartError(t, res.w)
	runID, state := f.onlyRun(t)
	if d[itemStartOutcomeCancelFailed] != runID.String() {
		t.Errorf("details = %v, want cancel_failed_run_id %s", d, runID)
	}
	if _, ok := d[itemStartOutcomeCancelled]; ok {
		t.Errorf("details carry cancelled_run_id for a failed cancel: %v", d)
	}
	if state != string(run.StatePending) {
		t.Errorf("run state = %q, want still pending", state)
	}
	f.assertAbandoned(t, runID, itemRunAbandonClaimConflict, false)
}

func TestStartClaimedCampaignItemRun_LinkFails_AbandonsLinkFailed(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.fc.linkErr = errors.New("link write failed")
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", res.w.Code, res.w.Body.String())
	}
	_, d := decodeItemStartError(t, res.w)
	runID, state := f.onlyRun(t)
	if d["run_id"] != runID.String() || d[itemStartOutcomeCancelled] != runID.String() {
		t.Errorf("details = %v, want run_id and cancelled_run_id %s (they survive 5xx redaction)", d, runID)
	}
	if state != string(run.StateCancelled) {
		t.Errorf("run state = %q, want cancelled", state)
	}
	if f.claimRows(t) != 0 {
		t.Error("claim not released after the link failure")
	}
	f.assertAbandoned(t, runID, itemRunAbandonLinkFailed, true)
}

func TestStartClaimedCampaignItemRun_TransitionFails_UnlinksAndAbandons(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.fc.transitionErr = errors.New("transition write failed")
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", res.w.Code, res.w.Body.String())
	}
	_, d := decodeItemStartError(t, res.w)
	runID, state := f.onlyRun(t)
	if d[itemStartOutcomeCancelled] != runID.String() {
		t.Errorf("details = %v, want cancelled_run_id %s", d, runID)
	}
	if state != string(run.StateCancelled) {
		t.Errorf("run state = %q, want cancelled", state)
	}
	if got := f.readItem(t); got.RunID != nil || got.State != campaign.ItemStatePending {
		t.Errorf("item run_id=%v state=%q, want unlinked pending", got.RunID, got.State)
	}
	f.assertAbandoned(t, runID, itemRunAbandonTransitionFailed, true)
}

func TestStartClaimedCampaignItemRun_AbandonAuditFails_StillCancelsAndReports(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	f.fc.linkErr = errors.New("link write failed")
	f.aud.fail.Store(true)
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	_, d := decodeItemStartError(t, res.w)
	runID, state := f.onlyRun(t)
	if state != string(run.StateCancelled) {
		t.Errorf("run state = %q, want cancelled even though the audit append failed", state)
	}
	if d[itemStartOutcomeCancelled] != runID.String() {
		t.Errorf("details = %v, want cancelled_run_id %s", d, runID)
	}
	if n := f.aud.count(categoryCampaignItemRunAbandoned); n != 0 {
		t.Errorf("abandoned rows = %d, want 0 (append failed)", n)
	}
}

func TestStartClaimedCampaignItemRun_ClientDisconnectMidMint_StartCompletes(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	f.gh.arm(cancelReq)
	var tailCtxErr error
	tailRan := false
	req := f.request(itemStartBudgets{})
	req.OnStarted = func(ctx context.Context, _ *run.Run, _ *campaign.Item) {
		tailRan = true
		tailCtxErr = ctx.Err()
	}
	res := f.callCore(t, reqCtx, req)
	if !res.ok {
		t.Fatalf("core ok = false after a client disconnect mid-mint: %d %s", res.w.Code, res.w.Body.String())
	}
	if reqCtx.Err() == nil {
		t.Fatal("the hook did not cancel the request context")
	}
	if got := f.readItem(t); got.RunID == nil || *got.RunID != res.run.ID || got.State != campaign.ItemStateRunning {
		t.Errorf("item run_id=%v state=%q, want linked running", got.RunID, got.State)
	}
	if !tailRan || tailCtxErr != nil {
		t.Errorf("post-start tail ran=%v ctxErr=%v, want it on the live detached start context (X1)", tailRan, tailCtxErr)
	}
}

// TestStartClaimedCampaignItemRun_MintOutlivesBudget_CleanupRunsOnItsOwnContext
// is the U3 test: the mint commits its run and returns only after the 2 s
// start budget expired, so the next database write (stage creation) fails on
// the expired start context. The cleanup must still release, cancel and
// record on its own context.
func TestStartClaimedCampaignItemRun_MintOutlivesBudget_CleanupRunsOnItsOwnContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelErr  error
		wantKey    string
		wantState  run.State
		wantCancel bool
	}{
		{"cancel_ok", nil, itemStartOutcomeCancelled, run.StateCancelled, true},
		{"cancel_fails", errors.New("cancel transition failed"), itemStartOutcomeCancelFailed, run.StatePending, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
			f.fr.slowCreateRun = true
			f.fr.cancelErr = tc.cancelErr
			res := f.callCore(t, nil, f.request(itemStartBudgets{Start: 2 * time.Second, Cleanup: 10 * time.Second}))
			if res.ok {
				t.Fatalf("core ok = true, want the expired start to fail")
			}
			code, d := decodeItemStartError(t, res.w)
			runID, state := f.onlyRun(t)
			if code != "campaign_run_start_incomplete" {
				t.Errorf("code = %q, want campaign_run_start_incomplete (stage creation is the first write after the mint)", code)
			}
			if d[tc.wantKey] != runID.String() {
				t.Errorf("details = %v, want %s %s", d, tc.wantKey, runID)
			}
			if state != string(tc.wantState) {
				t.Errorf("run state = %q, want %q", state, tc.wantState)
			}
			if f.claimRows(t) != 0 {
				t.Error("claim row remains: the release did not run on its own context")
			}
			f.assertAbandoned(t, runID, itemRunAbandonStartIncomplete, tc.wantCancel)
		})
	}
}

// TestStartClaimedCampaignItemRun_LockWaitConsumesMostOfTTL_StampsAfterLockAndStarts
// is the X4 lock-wait test: another transaction holds the item row lock while
// 9 of the claim's 10 minutes pass on the server clock. The claim is stamped
// from the clock read AFTER the lock is granted, so it still covers the start
// and cleanup budgets and the start proceeds.
func TestStartClaimedCampaignItemRun_LockWaitConsumesMostOfTTL_StampsAfterLockAndStarts(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM campaign_items WHERE id = $1 FOR UPDATE`, f.item.ID); err != nil {
		t.Fatalf("lock item: %v", err)
	}
	done := make(chan itemStartResult, 1)
	go func() { done <- f.callCore(t, nil, f.request(itemStartBudgets{})) }()
	waitForItemStartLockWaiter(t, f.pool, done)
	f.clock.advance(campaign.ItemStartClaimTTL - time.Minute)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	res := <-done
	if !res.ok {
		t.Fatalf("core ok = false after a 9-minute lock wait: %d %s", res.w.Code, res.w.Body.String())
	}
}

// waitForItemStartLockWaiter waits (bounded) for a backend waiting on a lock,
// failing when the start returned first.
func waitForItemStartLockWaiter(t *testing.T, pool *pgxpool.Pool, done <-chan itemStartResult) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case res := <-done:
			t.Fatalf("the start returned before waiting on the item lock: %d %s", res.w.Code, res.w.Body.String())
		default:
		}
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`,
		).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no backend began waiting on the item lock within 15s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartClaimedCampaignItemRun_ClaimShortOfBudgets_ReleasesAndRefuses pins
// X4's pre-mint coverage check: when the claim's remaining lifetime at the
// check no longer covers Start + Cleanup (the clock reads 6 minutes later
// than the claim's post-lock stamp), the core releases the claim and refuses
// start_in_progress without minting.
func TestStartClaimedCampaignItemRun_ClaimShortOfBudgets_ReleasesAndRefuses(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	var reads atomic.Int32
	f.s.nowFunc = func() time.Time {
		if reads.Add(1) == 1 {
			return itemStartT0 // the claim's single post-lock read
		}
		return itemStartT0.Add(6 * time.Minute)
	}
	f.gh.arm(nil)
	res := f.callCore(t, nil, f.request(itemStartBudgets{}))
	if res.w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", res.w.Code, res.w.Body.String())
	}
	code, d := decodeItemStartError(t, res.w)
	if code != "item_not_eligible" || d["start_in_progress"] != true || d["claim_budget_short"] != true {
		t.Errorf("code=%q details=%v, want item_not_eligible start_in_progress claim_budget_short", code, d)
	}
	if rows := f.runRows(t); len(rows) != 0 || f.gh.armedHits() != 0 {
		t.Errorf("runs=%v spec hits=%d, want nothing minted", rows, f.gh.armedHits())
	}
	if f.claimRows(t) != 0 {
		t.Error("the short claim was not released")
	}
}

func TestCancelCampaignRun_TransitionsToCancelled(t *testing.T) {
	f := newItemStartServerPG(t, newHookedSpecGitHub(t, gatedSpecYAML))
	ref := "issue:100"
	r, err := f.runs.CreateRun(context.Background(), run.CreateRunParams{Repo: f.c.Repo, WorkflowID: "feature_change", WorkflowSHA: "w", TriggerSource: run.TriggerGitHubIssue, TriggerRef: &ref})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := f.s.CancelCampaignRun(context.Background(), r.ID); err != nil {
		t.Fatalf("CancelCampaignRun: %v", err)
	}
	got, err := f.runs.GetRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.State != run.StateCancelled {
		t.Errorf("run state = %q, want cancelled", got.State)
	}
}
