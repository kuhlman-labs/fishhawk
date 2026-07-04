package campaigndriver

import (
	"bytes"
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

	listErr      error
	itemsErr     error
	linkErr      error
	transErr     error // injected error for TransitionCampaignItem (settle and start paths)
	pauseErr     error // injected error for PauseCampaignItem (page path)
	campTransErr error // injected error for TransitionCampaign (campaign-pause path)

	// recorded mutations
	itemTransitions []itemTransition
	campTransitions []campTransition
	links           []linkRecord
	pauses          []pauseRecord
}

type pauseRecord struct {
	itemID uuid.UUID
	reason campaign.PauseReason
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
	if f.transErr != nil {
		return nil, f.transErr
	}
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
	if f.campTransErr != nil {
		return nil, f.campTransErr
	}
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

func (f *fakeCampaignStore) PauseCampaignItem(_ context.Context, id uuid.UUID, reason campaign.PauseReason) (*campaign.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauseErr != nil {
		return nil, f.pauseErr
	}
	it := f.findItemLocked(id)
	if it == nil {
		return nil, campaign.ErrNotFound
	}
	if !campaign.ValidCampaignItemTransition(it.State, campaign.ItemStatePaused) {
		return nil, campaign.InvalidTransitionError{Kind: "campaign_item", From: string(it.State), To: string(campaign.ItemStatePaused)}
	}
	f.pauses = append(f.pauses, pauseRecord{itemID: id, reason: reason})
	r := reason
	it.State = campaign.ItemStatePaused
	it.PauseReason = &r
	return it, nil
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

// setState mutates a seeded run's state — the e2e test uses it to simulate the
// gate action (e.g. an auto-merge) driving the run toward terminal.
func (f *fakeRunReader) setState(id uuid.UUID, s run.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.runs[id]; ok {
		r.State = s
	}
}

// --- gate actor fake --------------------------------------------------------

// fakeGateActor records the runs handed to the GateActor seam and returns a
// scripted outcome (or error). drive lets a test vary the outcome per call and
// (in the e2e) advance the linked run to simulate the gate action's effect.
type fakeGateActor struct {
	mu        sync.Mutex
	calls     []uuid.UUID
	overrides [][]byte // campaign operator_agent override bytes per call (E25.12)
	err       error
	drive     func(runRow *run.Run) (GateActionOutcome, error)
}

// DriveRunGate is the base GateActor seam; driveGate only reaches it for a
// base-only actor. fakeGateActor implements the campaign-aware extension, so the
// driver calls DriveRunGateWithCampaign instead — this records a nil override.
func (f *fakeGateActor) DriveRunGate(ctx context.Context, runRow *run.Run) (GateActionOutcome, error) {
	return f.DriveRunGateWithCampaign(ctx, runRow, nil)
}

// DriveRunGateWithCampaign records the run and the campaign override bytes the
// driver threaded, so a test can assert driveGate passed c.OperatorAgent.
func (f *fakeGateActor) DriveRunGateWithCampaign(_ context.Context, runRow *run.Run, campaignOverride []byte) (GateActionOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, runRow.ID)
	f.overrides = append(f.overrides, campaignOverride)
	if f.err != nil {
		return GateActionOutcome{}, f.err
	}
	if f.drive != nil {
		return f.drive(runRow)
	}
	return GateActionOutcome{Note: "observe-only"}, nil
}

// lastOverride returns the campaign override bytes the most recent DriveRunGate
// call received, so a test can assert driveGate threaded c.OperatorAgent.
func (f *fakeGateActor) lastOverride() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.overrides) == 0 {
		return nil
	}
	return f.overrides[len(f.overrides)-1]
}

func (f *fakeGateActor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- notifier fake ----------------------------------------------------------

// fakeNotifier records the run ids the driver fired a page for via the Notifier
// seam (NotifyStatusUpdateForRun) and can inject an error.
type fakeNotifier struct {
	mu    sync.Mutex
	calls []uuid.UUID
	err   error
}

func (f *fakeNotifier) NotifyStatusUpdateForRun(_ context.Context, runID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, runID)
	return f.err
}

func (f *fakeNotifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
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

// TestTick_HumanLedItemNotStarted asserts the driver skips a human-led
// (autonomy:low) eligible item while still starting an autonomous eligible
// sibling (#1551). No driver code change is needed: NextEligible holds the
// human-led item out of Eligible, which the START pass keys on — so the
// human-led item is never dispatched.
func TestTick_HumanLedItemNotStarted(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	human := store.seedItem(c, "issue:41", campaign.ItemStatePending, nil, nil)
	human.Autonomy = "low" // human-led — must NOT be auto-started
	auto := store.seedItem(c, "issue:42", campaign.ItemStatePending, nil, nil)
	auto.Autonomy = "high" // agent-drivable — must be started
	reader := newFakeRunReader()
	starter := &fakeStarter{reader: reader}
	au := &fakeAudit{}
	tk := newTicker(store, reader, starter, au, 4)

	tk.Tick(context.Background())

	// Exactly one start — the autonomous sibling — and the human-led item is
	// untouched (no run, still pending).
	if len(starter.calls) != 1 || starter.calls[0] != auto.ID {
		t.Fatalf("expected one start for autonomous item %s, got %v", auto.ID, starter.calls)
	}
	if human.RunID != nil {
		t.Errorf("human-led item RunID = %v, want nil (never auto-started)", human.RunID)
	}
	if human.State != campaign.ItemStatePending {
		t.Errorf("human-led item state = %s, want pending (untouched)", human.State)
	}
	if auto.State != campaign.ItemStateRunning {
		t.Errorf("autonomous item state = %s, want running", auto.State)
	}
	started := au.byCategory(categoryCampaignIssueStarted)
	if len(started) != 1 || started[0].payload["issue_ref"] != "issue:42" {
		t.Fatalf("started audit = %+v, want single issue:42", started)
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

// start: a TransitionCampaignItem(running) failure AFTER the link committed
// must not strand the item. The driver rolls the link back (SetCampaignItemRun
// nil) so the next tick re-partitions the item as Eligible and retries, rather
// than leaving it linked-but-not-running — which NextEligible would classify as
// Running forever (never settled, never re-dispatched).
func TestTick_RunningTransitionError_RollsBackLinkAndRetries(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	it := store.seedItem(c, "issue:11", campaign.ItemStatePending, nil, nil)
	reader := newFakeRunReader()
	starter := &fakeStarter{reader: reader}
	au := &fakeAudit{}
	tk := newTicker(store, reader, starter, au, 4)

	// First tick: the link commits but the running transition fails.
	store.transErr = errors.New("transition boom")
	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStatePending {
		t.Fatalf("item state = %s, want pending (running transition failed)", it.State)
	}
	if it.RunID != nil {
		t.Fatal("item must be unlinked after a failed running transition so it stays retryable")
	}
	if au.count() != 0 {
		t.Fatalf("expected no started audit on transition failure, got %d", au.count())
	}
	if len(starter.calls) != 1 {
		t.Fatalf("expected exactly one start attempt on the first tick, got %d", len(starter.calls))
	}

	// Next tick with the transient error cleared: the now-Eligible item
	// re-dispatches and links cleanly.
	store.transErr = nil
	tk.Tick(context.Background())

	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running after retry", it.State)
	}
	if it.RunID == nil {
		t.Fatal("item must be linked after a successful retry")
	}
	if len(starter.calls) != 2 {
		t.Fatalf("expected a second start attempt on retry, got %d total", len(starter.calls))
	}
	if got := len(au.byCategory(categoryCampaignIssueStarted)); got != 1 {
		t.Fatalf("started audit entries after retry = %d, want 1", got)
	}
}

// advance: a TransitionCampaignItem failure while settling a terminal run is a
// logged continue — the item is NOT settled, settledAny stays false (so the
// campaign is not re-derived), and nothing is emitted. Symmetric with the
// GetRun / link / starter error branches; the item retries next tick.
func TestTick_SettleTransitionError_NoSettle(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	reader := newFakeRunReader()
	doneRun := reader.put(run.StateSucceeded)
	it := store.seedItem(c, "issue:4", campaign.ItemStateRunning, nil, &doneRun.ID)
	store.transErr = errors.New("settle transition boom")
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running (unsettled on transition error)", it.State)
	}
	if len(store.campTransitions) != 0 {
		t.Fatalf("campaign must not be re-derived when nothing settled, got %d transitions", len(store.campTransitions))
	}
	if au.count() != 0 {
		t.Fatalf("expected no audit on settle transition error, got %d", au.count())
	}
}

// --- E25.6 GATE ACTOR -------------------------------------------------------

// The ticker hands every running item whose linked run is NON-terminal to the
// GateActor seam during the ADVANCE pass, and records a campaign_gate_acted
// marker when the actor took an action. The run stays non-terminal this tick,
// so the item is NOT settled — that is the next tick's observation.
func TestTick_DrivesGateForRunningNonTerminalItem(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning) // non-terminal: parked at a gate
	it := store.seedItem(c, "issue:21", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	actor := &fakeGateActor{drive: func(_ *run.Run) (GateActionOutcome, error) {
		return GateActionOutcome{Acted: true, Action: "approve", Note: "auto-approved plan gate"}, nil
	}}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = actor

	tk.Tick(context.Background())

	if len(actor.calls) != 1 || actor.calls[0] != r.ID {
		t.Fatalf("actor calls = %v, want one call for run %s", actor.calls, r.ID)
	}
	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running (run still non-terminal, not settled)", it.State)
	}
	acted := au.byCategory(categoryCampaignGateActed)
	if len(acted) != 1 {
		t.Fatalf("campaign_gate_acted entries = %d, want 1", len(acted))
	}
	if acted[0].payload["action"] != "approve" ||
		acted[0].payload["run_id"] != r.ID.String() ||
		acted[0].payload["issue_ref"] != "issue:21" {
		t.Fatalf("campaign_gate_acted payload = %+v", acted[0].payload)
	}
}

// E25.12: driveGate threads the campaign's operator_agent override bytes to the
// GateActor seam so a campaign's issue-runs resolve their delegation against the
// campaign block. The bytes the actor receives must be exactly c.OperatorAgent.
func TestTick_DriveGate_ThreadsCampaignOperatorOverride(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	override := []byte(`{"may_retry":"infra_flake"}`)
	c.OperatorAgent = override
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning) // non-terminal: parked at a gate
	store.seedItem(c, "issue:31", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	actor := &fakeGateActor{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = actor

	tk.Tick(context.Background())

	if actor.callCount() != 1 {
		t.Fatalf("actor calls = %d, want 1", actor.callCount())
	}
	if got := actor.lastOverride(); !bytes.Equal(got, override) {
		t.Fatalf("override passed to actor = %q, want %q (driveGate must thread c.OperatorAgent)", got, override)
	}
}

// E25.12: a campaign with NO operator_agent override threads nil to the actor —
// the run then resolves on its own workflow contract (unchanged behavior).
func TestTick_DriveGate_NilCampaignOverride(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning) // OperatorAgent left nil
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	store.seedItem(c, "issue:32", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	actor := &fakeGateActor{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = actor

	tk.Tick(context.Background())

	if actor.callCount() != 1 {
		t.Fatalf("actor calls = %d, want 1", actor.callCount())
	}
	if got := actor.lastOverride(); got != nil {
		t.Fatalf("override passed to actor = %q, want nil (no campaign override)", got)
	}
}

// Auto-drive DISABLED (nil GateActor): the running non-terminal item is left
// parked — no actor call, no marker, no transition. The fail-closed
// observe-only contract.
func TestTick_AutoDriveDisabled_SkipsGate(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:22", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4) // GateActor nil

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running (parked, observe-only)", it.State)
	}
	if got := len(au.byCategory(categoryCampaignGateActed)); got != 0 {
		t.Fatalf("campaign_gate_acted entries = %d, want 0 with auto-drive disabled", got)
	}
	if au.count() != 0 {
		t.Fatalf("expected no audit entries observe-only, got %d", au.count())
	}
}

// pagingActor returns a GateActor that always pages with the given event — the
// must_page_human refusal the E25.7 pause/page branch acts on.
func pagingActor(event string) *fakeGateActor {
	return &fakeGateActor{drive: func(_ *run.Run) (GateActionOutcome, error) {
		return GateActionOutcome{Paged: true, PageEvent: event, Note: "must_page_human: " + event}, nil
	}}
}

// --- E25.7 PAUSE / PAGE -----------------------------------------------------

// (a) pause_campaign policy (the default): a must_page_human hand-off pauses
// the affected item AND the whole campaign, records exactly one campaign_paused
// marker (and NO campaign_gate_acted), and fires the human page once through
// the Notifier seam for the run.
func TestTick_GateActorPages_PauseCampaign_PausesItemAndCampaignAndPages(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	c.PausePolicy = campaign.PausePolicyPauseCampaign
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:23", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	notif := &fakeNotifier{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = pagingActor("reviewer_reject")
	tk.Notifier = notif

	tk.Tick(context.Background())

	if it.State != campaign.ItemStatePaused {
		t.Fatalf("item state = %s, want paused", it.State)
	}
	if it.PauseReason == nil || it.PauseReason.PageEvent != "reviewer_reject" || it.PauseReason.RunID == nil || *it.PauseReason.RunID != r.ID {
		t.Fatalf("item PauseReason = %+v, want page_event=reviewer_reject run_id=%s", it.PauseReason, r.ID)
	}
	if c.State != campaign.StatePaused {
		t.Fatalf("campaign state = %s, want paused (pause_campaign policy)", c.State)
	}
	if got := len(au.byCategory(categoryCampaignGateActed)); got != 0 {
		t.Fatalf("campaign_gate_acted entries = %d, want 0 on a page", got)
	}
	paused := au.byCategory(categoryCampaignPaused)
	if len(paused) != 1 {
		t.Fatalf("campaign_paused entries = %d, want 1", len(paused))
	}
	if paused[0].payload["issue_ref"] != "issue:23" ||
		paused[0].payload["run_id"] != r.ID.String() ||
		paused[0].payload["page_event"] != "reviewer_reject" ||
		paused[0].payload["policy"] != string(campaign.PausePolicyPauseCampaign) {
		t.Fatalf("campaign_paused payload = %+v", paused[0].payload)
	}
	if notif.callCount() != 1 || notif.calls[0] != r.ID {
		t.Fatalf("notifier calls = %v, want one page for run %s", notif.calls, r.ID)
	}
}

// (b) pause_item policy (continue-others): the affected item pauses but the
// campaign stays RUNNING so sibling items keep advancing.
func TestTick_GateActorPages_PauseItem_LeavesCampaignRunning(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	c.PausePolicy = campaign.PausePolicyPauseItem
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:24", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	notif := &fakeNotifier{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = pagingActor("requirement_arbitration")
	tk.Notifier = notif

	tk.Tick(context.Background())

	if it.State != campaign.ItemStatePaused {
		t.Fatalf("item state = %s, want paused", it.State)
	}
	if c.State != campaign.StateRunning {
		t.Fatalf("campaign state = %s, want running (pause_item policy keeps the campaign going)", c.State)
	}
	if len(store.campTransitions) != 0 {
		t.Fatalf("campaign must not transition under pause_item, got %d transitions", len(store.campTransitions))
	}
	paused := au.byCategory(categoryCampaignPaused)
	if len(paused) != 1 || paused[0].payload["policy"] != string(campaign.PausePolicyPauseItem) {
		t.Fatalf("campaign_paused entries = %+v, want one with pause_item policy", paused)
	}
	if notif.callCount() != 1 {
		t.Fatalf("notifier calls = %d, want 1", notif.callCount())
	}
}

// (c) nil Notifier (observe-only): the pause is still recorded but no page is
// fired — the seam is optional.
func TestTick_GateActorPages_NilNotifier_PausesWithoutPaging(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning) // zero policy → pause_campaign default
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:25", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = pagingActor("reviewer_reject")
	// tk.Notifier intentionally nil.

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStatePaused {
		t.Fatalf("item state = %s, want paused (recorded even with no notifier)", it.State)
	}
	if c.State != campaign.StatePaused {
		t.Fatalf("campaign state = %s, want paused (zero policy defaults to pause_campaign)", c.State)
	}
	if len(store.pauses) != 1 {
		t.Fatalf("pause records = %d, want 1", len(store.pauses))
	}
	if got := len(au.byCategory(categoryCampaignPaused)); got != 1 {
		t.Fatalf("campaign_paused entries = %d, want 1", got)
	}
}

// (c') a PauseCampaignItem error aborts the hand-off BEFORE the campaign pause
// and page — the safe outcome leaves the item running to retry next tick, the
// campaign untouched, and no page fired.
func TestTick_GateActorPages_PauseItemError_NoCampaignPauseNoPage(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	store.pauseErr = errors.New("pause boom")
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:26", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	notif := &fakeNotifier{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = pagingActor("reviewer_reject")
	tk.Notifier = notif

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running (pause failed, retries next tick)", it.State)
	}
	if c.State != campaign.StateRunning {
		t.Fatalf("campaign state = %s, want running (no campaign pause after item-pause error)", c.State)
	}
	if len(store.campTransitions) != 0 {
		t.Fatalf("campaign must not transition after a pause error, got %d", len(store.campTransitions))
	}
	if got := len(au.byCategory(categoryCampaignPaused)); got != 0 {
		t.Fatalf("campaign_paused entries = %d, want 0 on a pause error", got)
	}
	if notif.callCount() != 0 {
		t.Fatalf("notifier calls = %d, want 0 on a pause error", notif.callCount())
	}
}

// (d) sticky-paused: when one item pages (pause_campaign) and a SIBLING settles
// terminal in the SAME tick, the just-paused campaign must NOT be auto-unpaused
// by the re-derivation — resume is an explicit operator action.
func TestTick_GateActorPages_StickyPaused_NoAutoUnpause(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	c.PausePolicy = campaign.PausePolicyPauseCampaign
	reader := newFakeRunReader()
	// Pager: running item with a non-terminal run that pages.
	pageRun := reader.put(run.StateRunning)
	pager := store.seedItem(c, "issue:40", campaign.ItemStateRunning, nil, &pageRun.ID)
	// Sibling: running item whose run reached terminal this tick → settles.
	doneRun := reader.put(run.StateSucceeded)
	sibling := store.seedItem(c, "issue:41", campaign.ItemStateRunning, nil, &doneRun.ID)
	au := &fakeAudit{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = pagingActor("reviewer_reject")
	tk.Notifier = &fakeNotifier{}

	tk.Tick(context.Background())

	if pager.State != campaign.ItemStatePaused {
		t.Fatalf("pager item state = %s, want paused", pager.State)
	}
	if sibling.State != campaign.ItemStateSucceeded {
		t.Fatalf("sibling item state = %s, want succeeded (still settles)", sibling.State)
	}
	if c.State != campaign.StatePaused {
		t.Fatalf("campaign state = %s, want paused (sticky, not auto-unpaused by the sibling settle)", c.State)
	}
	if got := len(au.byCategory(categoryCampaignAdvanced)); got != 0 {
		t.Fatalf("campaign_advanced entries = %d, want 0 (a paused campaign is not re-derived)", got)
	}
}

// (e) campaign-pause transition error (pause_campaign): the item already paused,
// so the failed campaign transition only degrades — it does NOT unwind the item
// pause, the campaign_paused marker is still recorded, and the page still fires.
func TestTick_GateActorPages_CampaignPauseError_ItemPausedPagesFired(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	c.PausePolicy = campaign.PausePolicyPauseCampaign
	store.campTransErr = errors.New("campaign pause boom")
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:27", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	notif := &fakeNotifier{}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = pagingActor("reviewer_reject")
	tk.Notifier = notif

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStatePaused {
		t.Fatalf("item state = %s, want paused (item pause must not unwind on a campaign-pause error)", it.State)
	}
	if c.State != campaign.StateRunning {
		t.Fatalf("campaign state = %s, want running (the campaign transition failed)", c.State)
	}
	if got := len(au.byCategory(categoryCampaignPaused)); got != 1 {
		t.Fatalf("campaign_paused entries = %d, want 1 (recorded despite the campaign-pause error)", got)
	}
	if notif.callCount() != 1 {
		t.Fatalf("notifier calls = %d, want 1 (page still fires)", notif.callCount())
	}
}

// (f) Notifier error: a page failure WARN-logs and does NOT unwind the recorded
// pause — the safe outcome (the pause) survives a transient page error.
func TestTick_GateActorPages_NotifierError_PauseSurvives(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	c.PausePolicy = campaign.PausePolicyPauseItem
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:28", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	notif := &fakeNotifier{err: errors.New("page post boom")}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = pagingActor("reviewer_reject")
	tk.Notifier = notif

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStatePaused {
		t.Fatalf("item state = %s, want paused (pause survives a page error)", it.State)
	}
	if got := len(au.byCategory(categoryCampaignPaused)); got != 1 {
		t.Fatalf("campaign_paused entries = %d, want 1", got)
	}
	if notif.callCount() != 1 {
		t.Fatalf("notifier calls = %d, want 1 (the page was attempted)", notif.callCount())
	}
}

// A GateActor error is a logged continue — no marker, the item stays parked and
// retries next tick (mirrors the per-item transient-error posture).
func TestTick_GateActorError_LeavesParked(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	reader := newFakeRunReader()
	r := reader.put(run.StateRunning)
	it := store.seedItem(c, "issue:24", campaign.ItemStateRunning, nil, &r.ID)
	au := &fakeAudit{}
	actor := &fakeGateActor{err: errors.New("dispatch boom")}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = actor

	tk.Tick(context.Background()) // must not panic

	if it.State != campaign.ItemStateRunning {
		t.Fatalf("item state = %s, want running (parked on actor error)", it.State)
	}
	if au.count() != 0 {
		t.Fatalf("expected no audit on actor error, got %d", au.count())
	}
	if actor.callCount() != 1 {
		t.Fatalf("actor calls = %d, want 1", actor.callCount())
	}
}

// END-TO-END (driver -> gate actor -> marker -> terminal settle): a clean run is
// auto-approved at its plan gate (tick 1), auto-merged at its review gate (tick
// 2, which drives the run terminal), then settled with the campaign advanced to
// succeeded (tick 3). An injected reviewer_reject on a SECOND campaign item does
// NOT auto-act and leaves that item parked — the actor's campaign_gate_paged
// hand-off is on the run chain, so the driver records no campaign_gate_acted for
// it. Together this exercises the full advance->drive->record->settle path the
// driver owns; the actor's real delegation->action crossing is covered in
// server/autodrive_test.go and the serve wiring in serve_test.go.
func TestTick_EndToEnd_AutoActsThenSettles_AndPagesReject(t *testing.T) {
	store := newFakeStore()
	c := store.seedCampaign(campaign.StateRunning)
	// pause_item so the reject's hand-off pauses only that item; the campaign
	// keeps running and the clean item auto-acts across the three ticks.
	c.PausePolicy = campaign.PausePolicyPauseItem
	reader := newFakeRunReader()

	cleanRun := reader.put(run.StateRunning)  // auto-driven to terminal
	rejectRun := reader.put(run.StateRunning) // pages, stays parked
	clean := store.seedItem(c, "issue:30", campaign.ItemStateRunning, nil, &cleanRun.ID)
	reject := store.seedItem(c, "issue:31", campaign.ItemStateRunning, nil, &rejectRun.ID)
	au := &fakeAudit{}

	cleanCalls := 0
	actor := &fakeGateActor{drive: func(rr *run.Run) (GateActionOutcome, error) {
		if rr.ID == rejectRun.ID {
			// reviewer_reject must_page_human: refuse, no action.
			return GateActionOutcome{Paged: true, PageEvent: "reviewer_reject"}, nil
		}
		cleanCalls++
		switch cleanCalls {
		case 1:
			return GateActionOutcome{Acted: true, Action: "approve"}, nil
		case 2:
			// auto-merge drives the run terminal; settled next tick.
			reader.setState(rr.ID, run.StateSucceeded)
			return GateActionOutcome{Acted: true, Action: "merge"}, nil
		default:
			return GateActionOutcome{Note: "observe-only"}, nil
		}
	}}
	tk := newTicker(store, reader, &fakeStarter{reader: reader}, au, 4)
	tk.GateActor = actor

	tk.Tick(context.Background()) // tick 1: approve clean; page reject
	tk.Tick(context.Background()) // tick 2: merge clean (-> terminal); page reject
	tk.Tick(context.Background()) // tick 3: settle clean

	// The clean run was auto-acted twice (approve, then merge).
	acted := au.byCategory(categoryCampaignGateActed)
	if len(acted) != 2 {
		t.Fatalf("campaign_gate_acted entries = %d, want 2 (approve, merge)", len(acted))
	}
	if acted[0].payload["action"] != "approve" || acted[1].payload["action"] != "merge" {
		t.Fatalf("acted actions = [%v, %v], want [approve, merge]", acted[0].payload["action"], acted[1].payload["action"])
	}
	for _, a := range acted {
		if a.payload["run_id"] != cleanRun.ID.String() {
			t.Fatalf("campaign_gate_acted run_id = %v, want clean run %s", a.payload["run_id"], cleanRun.ID)
		}
	}

	// The clean item settled succeeded once the auto-merge drove the run terminal.
	if clean.State != campaign.ItemStateSucceeded {
		t.Fatalf("clean item state = %s, want succeeded", clean.State)
	}
	if settled := au.byCategory(categoryCampaignIssueSettled); len(settled) != 1 ||
		settled[0].payload["run_id"] != cleanRun.ID.String() {
		t.Fatalf("settled entries = %+v, want one for the clean run", settled)
	}

	// The reject item never auto-acted; it was paused on the hand-off (E25.7)
	// and recorded NO campaign_gate_acted. With pause_item policy the campaign
	// itself stayed running so the clean item could finish.
	if reject.State != campaign.ItemStatePaused {
		t.Fatalf("reject item state = %s, want paused (gate handed off)", reject.State)
	}
	if got := len(au.byCategory(categoryCampaignPaused)); got != 1 {
		t.Fatalf("campaign_paused entries = %d, want 1 (one hand-off)", got)
	}
}
