package campaigndriver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fixedNow is the deterministic clock injected into every Ticker under test.
func fixedNow() time.Time { return time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC) }

// --- stateful campaign store fake -------------------------------------------

type fakeCampaignStore struct {
	campaign.BaseFake
	mu        sync.Mutex
	campaigns map[uuid.UUID]*campaign.Campaign
	items     map[uuid.UUID][]*campaign.Item // keyed by campaign id

	listErr  error
	itemsErr error
	linkErr  error

	// recorded mutations
	itemTransitions []itemTransition
	campTransitions []campTransition
	links           []linkRecord
}

type itemTransition struct {
	itemID uuid.UUID
	to     campaign.ItemState
}
type campTransition struct {
	campaignID uuid.UUID
	to         campaign.State
}
type linkRecord struct {
	itemID uuid.UUID
	runID  *uuid.UUID
}

func newFakeStore() *fakeCampaignStore {
	return &fakeCampaignStore{
		campaigns: map[uuid.UUID]*campaign.Campaign{},
		items:     map[uuid.UUID][]*campaign.Item{},
	}
}

func (f *fakeCampaignStore) seedCampaign(state campaign.State) *campaign.Campaign {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &campaign.Campaign{ID: uuid.New(), Repo: "x/y", EpicRef: "issue:1", State: state}
	f.campaigns[c.ID] = c
	return c
}

func (f *fakeCampaignStore) seedItem(c *campaign.Campaign, ref string, state campaign.ItemState, deps []string, runID *uuid.UUID) *campaign.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := &campaign.Item{ID: uuid.New(), CampaignID: c.ID, IssueRef: ref, DependsOn: deps, State: state, RunID: runID}
	f.items[c.ID] = append(f.items[c.ID], it)
	return it
}

func (f *fakeCampaignStore) ListCampaigns(_ context.Context, fil campaign.ListCampaignsFilter) ([]*campaign.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*campaign.Campaign
	for _, c := range f.campaigns {
		if fil.State != "" && string(c.State) != fil.State {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeCampaignStore) ListCampaignItemsForCampaign(_ context.Context, id uuid.UUID) ([]*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.itemsErr != nil {
		return nil, f.itemsErr
	}
	// Return a snapshot of pointers so the driver observes live state.
	src := f.items[id]
	out := make([]*campaign.Item, len(src))
	copy(out, src)
	return out, nil
}

func (f *fakeCampaignStore) SetCampaignItemRun(_ context.Context, itemID uuid.UUID, runID *uuid.UUID) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linkErr != nil {
		return nil, f.linkErr
	}
	f.links = append(f.links, linkRecord{itemID: itemID, runID: runID})
	it := f.findItemLocked(itemID)
	if it == nil {
		return nil, campaign.ErrNotFound
	}
	it.RunID = runID
	return it, nil
}

func (f *fakeCampaignStore) TransitionCampaignItem(_ context.Context, id uuid.UUID, to campaign.ItemState) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.findItemLocked(id)
	if it == nil {
		return nil, campaign.ErrNotFound
	}
	if !campaign.ValidCampaignItemTransition(it.State, to) {
		return nil, campaign.InvalidTransitionError{Kind: "campaign_item", From: string(it.State), To: string(to)}
	}
	f.itemTransitions = append(f.itemTransitions, itemTransition{itemID: id, to: to})
	it.State = to
	return it, nil
}

func (f *fakeCampaignStore) TransitionCampaign(_ context.Context, id uuid.UUID, to campaign.State) (*campaign.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.campaigns[id]
	if !ok {
		return nil, campaign.ErrNotFound
	}
	if !campaign.ValidCampaignTransition(c.State, to) {
		return nil, campaign.InvalidTransitionError{Kind: "campaign", From: string(c.State), To: string(to)}
	}
	f.campTransitions = append(f.campTransitions, campTransition{campaignID: id, to: to})
	c.State = to
	return c, nil
}

func (f *fakeCampaignStore) findItemLocked(id uuid.UUID) *campaign.Item {
	for _, items := range f.items {
		for _, it := range items {
			if it.ID == id {
				return it
			}
		}
	}
	return nil
}

// --- run reader fake --------------------------------------------------------

type fakeRunReader struct {
	mu      sync.Mutex
	runs    map[uuid.UUID]*run.Run
	getErr  error
	getHits int
}

func newFakeRunReader() *fakeRunReader {
	return &fakeRunReader{runs: map[uuid.UUID]*run.Run{}}
}

func (f *fakeRunReader) put(state run.State) *run.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &run.Run{ID: uuid.New(), State: state}
	f.runs[r.ID] = r
	return r
}

func (f *fakeRunReader) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getHits++
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

// --- run starter fake -------------------------------------------------------

type fakeStarter struct {
	mu       sync.Mutex
	calls    []uuid.UUID // item ids started, in order
	startErr error
	reader   *fakeRunReader // newly-created runs land here so advance can read them
}

func (f *fakeStarter) StartCampaignRun(_ context.Context, item *campaign.Item, _ *campaign.Campaign) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.calls = append(f.calls, item.ID)
	r := &run.Run{ID: uuid.New(), State: run.StatePending}
	if f.reader != nil {
		f.reader.runs[r.ID] = r
	}
	return r, nil
}

// --- audit recorder fake ----------------------------------------------------

type recordedAudit struct {
	category string
	payload  map[string]any
}

type fakeAudit struct {
	audit.BaseFake
	mu        sync.Mutex
	entries   []recordedAudit
	appendErr error
}

func (f *fakeAudit) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	var payload map[string]any
	_ = json.Unmarshal(p.Payload, &payload)
	f.entries = append(f.entries, recordedAudit{category: p.Category, payload: payload})
	return &audit.Entry{}, nil
}

func (f *fakeAudit) byCategory(cat string) []recordedAudit {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedAudit
	for _, e := range f.entries {
		if e.category == cat {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeAudit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

// newTicker wires a Ticker with the supplied fakes, the deterministic clock,
// and a discarding logger.
func newTicker(store *fakeCampaignStore, reader *fakeRunReader, starter RunStarter, au *fakeAudit, maxParallel int) *Ticker {
	return &Ticker{
		Campaigns:   store,
		Runs:        reader,
		Starter:     starter,
		Audit:       au,
		MaxParallel: maxParallel,
		Now:         fixedNow,
	}
}

// --- (b) FAIL-CLOSED: a nil required dependency is a logged no-op -----------

func TestTick_NilDependency_NoPanicNoStart(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	store.seedItem(c, "issue:10", campaign.ItemStatePending, nil, nil)
	starter := &fakeStarter{}

	// Nil Starter: Tick must not panic, must not start a run, must not transition.
	tk := &Ticker{Campaigns: store, Runs: newFakeRunReader(), Starter: nil, Audit: &fakeAudit{}, Now: fixedNow}
	tk.Tick(context.Background()) // must not panic

	if len(starter.calls) != 0 {
		t.Fatalf("expected no starts with nil dependency, got %d", len(starter.calls))
	}
	if len(store.itemTransitions) != 0 {
		t.Fatalf("expected no item transitions with nil dependency, got %d", len(store.itemTransitions))
	}

	// Run() must reject a nil dependency rather than spin.
	if err := (&Ticker{Campaigns: store, Runs: newFakeRunReader(), Audit: &fakeAudit{}}).Run(context.Background()); err == nil {
		t.Fatal("expected Run to error on a nil required dependency")
	}
}

// --- (c) CONCURRENCY CAP ----------------------------------------------------

func TestTick_ConcurrencyCap(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	// 5 independent eligible items, no running items, cap 2 → start exactly 2.
	for _, ref := range []string{"issue:1", "issue:2", "issue:3", "issue:4", "issue:5"} {
		store.seedItem(c, ref, campaign.ItemStatePending, nil, nil)
	}
	reader := newFakeRunReader()
	starter := &fakeStarter{reader: reader}
	au := &fakeAudit{}
	tk := newTicker(store, reader, starter, au, 2)

	tk.Tick(context.Background())

	if len(starter.calls) != 2 {
		t.Fatalf("concurrency cap: started %d, want 2", len(starter.calls))
	}
	if got := len(au.byCategory(categoryCampaignIssueStarted)); got != 2 {
		t.Fatalf("started audit entries = %d, want 2", got)
	}
	runningCount := 0
	for _, tr := range store.itemTransitions {
		if tr.to == campaign.ItemStateRunning {
			runningCount++
		}
	}
	if runningCount != 2 {
		t.Fatalf("running transitions = %d, want 2", runningCount)
	}
}

// --- (d) START PATH ---------------------------------------------------------

func TestTick_StartPath(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	it := store.seedItem(c, "issue:42", campaign.ItemStatePending, nil, nil)
	reader := newFakeRunReader()
	starter := &fakeStarter{reader: reader}
	au := &fakeAudit{}
	tk := newTicker(store, reader, starter, au, 4)

	tk.Tick(context.Background())

	if len(starter.calls) != 1 || starter.calls[0] != it.ID {
		t.Fatalf("expected one start for item %s, got %v", it.ID, starter.calls)
	}
	if len(store.links) != 1 || store.links[0].itemID != it.ID || store.links[0].runID == nil {
		t.Fatalf("expected a run link for item, got %+v", store.links)
	}
	if it.RunID == nil {
		t.Fatal("item RunID not set after start")
	}
	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running", it.State)
	}
	started := au.byCategory(categoryCampaignIssueStarted)
	if len(started) != 1 {
		t.Fatalf("started audit entries = %d, want 1", len(started))
	}
	if started[0].payload["issue_ref"] != "issue:42" {
		t.Fatalf("started audit issue_ref = %v, want issue:42", started[0].payload["issue_ref"])
	}
	if started[0].payload["run_id"] != it.RunID.String() {
		t.Fatalf("started audit run_id = %v, want %s", started[0].payload["run_id"], it.RunID.String())
	}
}

// --- (e) ADVANCE PATH (settle + campaign advance) ---------------------------

func TestTick_AdvancePath_SettlesAndAdvancesCampaign(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	reader := newFakeRunReader()
	doneRun := reader.put(run.StateSucceeded)
	// Single running item linked to a terminal-succeeded run.
	it := store.seedItem(c, "issue:7", campaign.ItemStateRunning, nil, &doneRun.ID)
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)

	tk.Tick(context.Background())

	if it.State != campaign.ItemStateSucceeded {
		t.Fatalf("item state = %s, want succeeded", it.State)
	}
	settled := au.byCategory(categoryCampaignIssueSettled)
	if len(settled) != 1 || settled[0].payload["outcome"] != string(campaign.ItemStateSucceeded) {
		t.Fatalf("settled audit = %+v, want one succeeded outcome", settled)
	}
	// Single-item campaign all-succeeded → campaign running → succeeded.
	if c.State != campaign.StateSucceeded {
		t.Fatalf("campaign state = %s, want succeeded", c.State)
	}
	advanced := au.byCategory(categoryCampaignAdvanced)
	if len(advanced) != 1 || advanced[0].payload["to"] != string(campaign.StateSucceeded) {
		t.Fatalf("advanced audit = %+v, want one running→succeeded", advanced)
	}
}

// --- (f) TERMINAL-STATE MAPPING (one assertion per branch) ------------------

func TestTick_TerminalStateMapping(t *testing.T) {
	cases := []struct {
		name     string
		runState run.State
		wantItem campaign.ItemState
	}{
		{"succeeded", run.StateSucceeded, campaign.ItemStateSucceeded},
		{"failed", run.StateFailed, campaign.ItemStateFailed},
		{"cancelled", run.StateCancelled, campaign.ItemStateCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			c := store.seedCampaign(campaign.StateRunning)
			reader := newFakeRunReader()
			r := reader.put(tc.runState)
			it := store.seedItem(c, "issue:9", campaign.ItemStateRunning, nil, &r.ID)
			au := &fakeAudit{}
			tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)

			tk.Tick(context.Background())

			if it.State != tc.wantItem {
				t.Fatalf("item state = %s, want %s", it.State, tc.wantItem)
			}
		})
	}
}

// --- (g) IDEMPOTENCY --------------------------------------------------------

func TestTick_Idempotency_AlreadySettled(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateSucceeded) // already terminal
	reader := newFakeRunReader()
	r := reader.put(run.StateSucceeded)
	store.seedItem(c, "issue:7", campaign.ItemStateSucceeded, nil, &r.ID)
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)

	// A terminal campaign is not listed (filter on running), so a re-tick is
	// wholly inert.
	tk.Tick(context.Background())

	if len(store.itemTransitions) != 0 || len(store.campTransitions) != 0 {
		t.Fatalf("expected no transitions on re-tick, got items=%d camps=%d",
			len(store.itemTransitions), len(store.campTransitions))
	}
	if au.count() != 0 {
		t.Fatalf("expected no audit on re-tick, got %d", au.count())
	}
}

func TestTick_Idempotency_RunningCampaignNoNewWork(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	reader := newFakeRunReader()
	// One item already running with a still-in-flight run, one already
	// succeeded. Nothing to settle, nothing newly eligible.
	inflight := reader.put(run.StateRunning)
	store.seedItem(c, "issue:1", campaign.ItemStateRunning, nil, &inflight.ID)
	doneRun := reader.put(run.StateSucceeded)
	store.seedItem(c, "issue:2", campaign.ItemStateSucceeded, nil, &doneRun.ID)
	au := &fakeAudit{}
	starter := &fakeStarter{reader: reader}
	tk := newTicker(store, reader, starter, au, 4)

	tk.Tick(context.Background())

	if len(starter.calls) != 0 {
		t.Fatalf("expected no starts, got %d", len(starter.calls))
	}
	if len(store.itemTransitions) != 0 {
		t.Fatalf("expected no item transitions, got %d", len(store.itemTransitions))
	}
	if au.count() != 0 {
		t.Fatalf("expected no audit entries, got %d", au.count())
	}
}

// --- transient error tolerance ----------------------------------------------

func TestTick_StarterError_LeavesItemUnstarted(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	it := store.seedItem(c, "issue:5", campaign.ItemStatePending, nil, nil)
	reader := newFakeRunReader()
	starter := &fakeStarter{reader: reader, startErr: errors.New("boom")}
	au := &fakeAudit{}
	tk := newTicker(store, reader, starter, au, 4)

	tk.Tick(context.Background()) // must not panic

	if it.RunID != nil {
		t.Fatal("item must stay unlinked when the starter errors")
	}
	if it.State != campaign.ItemStatePending {
		t.Fatalf("item state = %s, want pending (unchanged)", it.State)
	}
	if au.count() != 0 {
		t.Fatalf("expected no audit on start failure, got %d", au.count())
	}
}

func TestTick_ListCampaignsError_NoPanic(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("db down")
	tk := newTicker(store, newFakeRunReader(), &fakeStarter{}, &fakeAudit{}, 4)
	tk.Tick(context.Background()) // logged no-op, no panic
}

// advance: a GetRun error on a running item's linked run is a logged
// continue — the item is NOT settled and nothing is emitted.
func TestTick_GetRunError_NoSettle(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	reader := newFakeRunReader()
	r := reader.put(run.StateSucceeded)
	it := store.seedItem(c, "issue:3", campaign.ItemStateRunning, nil, &r.ID)
	reader.getErr = errors.New("run read failed")
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running (unsettled on GetRun error)", it.State)
	}
	if au.count() != 0 {
		t.Fatalf("expected no audit on GetRun error, got %d", au.count())
	}
}

// start: a SetCampaignItemRun (link) error is a logged continue — the item
// is NOT transitioned to running and no started audit is emitted (no partial
// commit of a half-started item).
func TestTick_LinkError_NoRunningTransition(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	it := store.seedItem(c, "issue:8", campaign.ItemStatePending, nil, nil)
	store.linkErr = errors.New("link failed")
	reader := newFakeRunReader()
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)

	tk.Tick(context.Background()) // must not panic

	if it.State == campaign.ItemStateRunning {
		t.Fatal("item must NOT transition to running when the link fails")
	}
	if len(store.itemTransitions) != 0 {
		t.Fatalf("expected no item transitions on link error, got %d", len(store.itemTransitions))
	}
	if au.count() != 0 {
		t.Fatalf("expected no started audit on link error, got %d", au.count())
	}
}
