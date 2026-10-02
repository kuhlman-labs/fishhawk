package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

const testRepo = "kuhlman-labs/fishhawk"

// hourlySpec declares `upkeep` on an hourly UTC cadence, plus a sibling
// `manual` that opts in to the scheduled trigger but declares NO schedule —
// the scheduler must never start it.
var hourlySpec = []byte(`
version: "2"
workflows:
  upkeep:
    applies_to:
      trigger: [scheduled, on_demand]
    schedule:
      cron: "0 * * * *"
      issue: 3725
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
  manual:
    applies_to:
      trigger: [scheduled]
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`)

var (
	w1 = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	w2 = w1.Add(time.Hour)
)

// fakeSpecs serves one spec per repo, or an error.
type fakeSpecs struct {
	mu      sync.Mutex
	content map[string][]byte
	err     map[string]error
	calls   int
}

func (f *fakeSpecs) FetchSpec(_ context.Context, repo string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.err[repo]; err != nil {
		return nil, "", err
	}
	c, ok := f.content[repo]
	if !ok {
		return nil, "", errors.New("no spec")
	}
	return c, "blobsha-" + repo, nil
}

// fakeStarter mirrors handleCreateRun's Idempotency-Key semantics: a
// non-empty key that matches a prior run REPLAYS it (already_started); an
// EMPTY key is non-idempotent and always mints a new run. Scripted results
// (refuse / transient error / bogus kind) override the default for the next
// calls in order.
type fakeStarter struct {
	mu       sync.Mutex
	byKey    map[string]string // (repo + "\x00" + key) → run id
	runs     []StartRequest    // every minted run
	calls    []StartRequest
	scripted []func(StartRequest) (StartOutcome, error)
}

func (f *fakeStarter) StartScheduledRun(_ context.Context, req StartRequest) (StartOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if len(f.scripted) > 0 {
		next := f.scripted[0]
		f.scripted = f.scripted[1:]
		return next(req)
	}
	if req.IdempotencyKey != "" {
		if id, ok := f.byKey[req.Repo+"\x00"+req.IdempotencyKey]; ok {
			return StartOutcome{Kind: OutcomeAlreadyStarted, RunID: id}, nil
		}
	}
	id := fmt.Sprintf("run-%d", len(f.runs)+1)
	f.runs = append(f.runs, req)
	if req.IdempotencyKey != "" {
		if f.byKey == nil {
			f.byKey = map[string]string{}
		}
		f.byKey[req.Repo+"\x00"+req.IdempotencyKey] = id
	}
	return StartOutcome{Kind: OutcomeStarted, RunID: id}, nil
}

func (f *fakeStarter) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeStarter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeAudit records global-chain appends.
type fakeAudit struct {
	mu      sync.Mutex
	entries []audit.GlobalChainAppendParams
	err     error
}

func (f *fakeAudit) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.entries = append(f.entries, p)
	return &audit.Entry{Category: p.Category}, nil
}

func (f *fakeAudit) categories() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.Category)
	}
	return out
}

func (f *fakeAudit) payload(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.entries) {
		t.Fatalf("audit entry %d absent; have %d", i, len(f.entries))
	}
	var m map[string]any
	if err := json.Unmarshal(f.entries[i].Payload, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m
}

// fakeClock is a settable Now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type harness struct {
	specs   *fakeSpecs
	starter *fakeStarter
	audit   *fakeAudit
	clock   *fakeClock
}

func newHarness() *harness {
	return &harness{
		specs:   &fakeSpecs{content: map[string][]byte{testRepo: hourlySpec}, err: map[string]error{}},
		starter: &fakeStarter{},
		audit:   &fakeAudit{},
		clock:   &fakeClock{t: w1.Add(5 * time.Minute)},
	}
}

// ticker builds a FRESH Ticker over the harness's shared fakes — a new one
// is the simulated restart: same database (starter + audit), empty
// in-memory attempted mark.
func (h *harness) ticker(runnerKind string) *Ticker {
	return &Ticker{
		Repos:      []string{testRepo},
		Specs:      h.specs,
		Starter:    h.starter,
		Audit:      h.audit,
		RunnerKind: runnerKind,
		Now:        h.clock.Now,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIdempotencyKey_UTCRFC3339(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	got := IdempotencyKey("upkeep", time.Date(2026, 10, 2, 5, 0, 0, 0, chicago))
	if want := "scheduled:upkeep:2026-10-02T10:00:00Z"; got != want {
		t.Errorf("IdempotencyKey = %q, want %q", got, want)
	}
}

// TestTick_TwoWindowsAndRestart_ExactlyTwoRuns is the exactly-once proof at
// unit level: W1 starts once, a second tick in W1 is silent, a RESTARTED
// ticker in W1 records skipped, and W2 starts the second run.
//
// COUNTERFACTUAL (approval condition 6): mutate IdempotencyKey's BODY to
// return "" and this test goes RED with three runs, not two. The fixture
// state that makes the deletion observable: the restarted ticker (tB) has an
// EMPTY in-memory attempted map, so the only thing standing between its W1
// tick and a second W1 run is fakeStarter's replay on a non-empty key; with
// the key empty the fake mints (exactly as handleCreateRun skips the replay
// lookup on an empty Idempotency-Key header).
func TestTick_TwoWindowsAndRestart_ExactlyTwoRuns(t *testing.T) {
	h := newHarness()
	ctx := context.Background()

	tA := h.ticker(run.RunnerKindGitHubActions)
	tA.Tick(ctx) // W1 +5m → started
	h.clock.Set(w1.Add(30 * time.Minute))
	tA.Tick(ctx) // still W1, same process → nothing

	tB := h.ticker(run.RunnerKindGitHubActions) // restart
	h.clock.Set(w1.Add(45 * time.Minute))
	tB.Tick(ctx) // W1 again, fresh process → replay → skipped
	h.clock.Set(w2.Add(5 * time.Minute))
	tB.Tick(ctx) // W2 → started

	if got := h.starter.runCount(); got != 2 {
		t.Fatalf("runs minted = %d, want exactly 2 (one per window)", got)
	}
	keys := []string{h.starter.runs[0].IdempotencyKey, h.starter.runs[1].IdempotencyKey}
	wantKeys := []string{"scheduled:upkeep:2026-10-02T10:00:00Z", "scheduled:upkeep:2026-10-02T11:00:00Z"}
	if !equalStrings(keys, wantKeys) {
		t.Errorf("run idempotency keys = %v, want %v", keys, wantKeys)
	}
	wantCats := []string{CategoryScheduledRunStarted, CategoryScheduledRunSkipped, CategoryScheduledRunStarted}
	if got := h.audit.categories(); !equalStrings(got, wantCats) {
		t.Errorf("audit sequence = %v, want %v", got, wantCats)
	}
	if p := h.audit.payload(t, 1); p["run_id"] != "run-1" || p["reason"] != SkipReasonAlreadyStarted {
		t.Errorf("skipped payload = %v, want run_id run-1 and reason already_started", p)
	}
}

func TestTick_Started_RecordsEntryAndRequest(t *testing.T) {
	h := newHarness()
	h.ticker(run.RunnerKindGitHubActions).Tick(context.Background())

	if got := h.starter.callCount(); got != 1 {
		t.Fatalf("starter calls = %d, want 1 (only the scheduled workflow)", got)
	}
	req := h.starter.calls[0]
	if req.Repo != testRepo || req.WorkflowID != "upkeep" || req.IssueNumber != 3725 ||
		req.WorkflowSHA != "blobsha-"+testRepo || string(req.WorkflowSpec) != string(hourlySpec) ||
		req.RunnerKind != run.RunnerKindGitHubActions ||
		req.IdempotencyKey != "scheduled:upkeep:2026-10-02T10:00:00Z" {
		t.Errorf("start request = %+v", req)
	}

	if got := h.audit.categories(); !equalStrings(got, []string{CategoryScheduledRunStarted}) {
		t.Fatalf("audit = %v, want [scheduled_run_started]", got)
	}
	e := h.audit.entries[0]
	if e.ActorKind == nil || *e.ActorKind != audit.ActorSystem || e.ActorSubject == nil || *e.ActorSubject != ActorSubject {
		t.Errorf("actor = %v/%v, want system/%s", e.ActorKind, e.ActorSubject, ActorSubject)
	}
	p := h.audit.payload(t, 0)
	want := map[string]any{
		"repo":            testRepo,
		"workflow_id":     "upkeep",
		"window_start":    "2026-10-02T10:00:00Z",
		"idempotency_key": "scheduled:upkeep:2026-10-02T10:00:00Z",
		"runner_kind":     run.RunnerKindGitHubActions,
		"run_id":          "run-1",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("payload[%s] = %v, want %v", k, p[k], v)
		}
	}
	if _, ok := p["dispatch_note"]; ok {
		t.Errorf("github_actions payload carries dispatch_note: %v", p)
	}
}

func TestTick_AlreadyStarted_RecordsSkipped(t *testing.T) {
	h := newHarness()
	h.starter.scripted = append(h.starter.scripted, func(StartRequest) (StartOutcome, error) {
		return StartOutcome{Kind: OutcomeAlreadyStarted, RunID: "run-prior"}, nil
	})
	tk := h.ticker(run.RunnerKindGitHubActions)
	tk.Tick(context.Background())

	if got := h.audit.categories(); !equalStrings(got, []string{CategoryScheduledRunSkipped}) {
		t.Fatalf("audit = %v, want [scheduled_run_skipped]", got)
	}
	p := h.audit.payload(t, 0)
	if p["run_id"] != "run-prior" || p["reason"] != SkipReasonAlreadyStarted {
		t.Errorf("skipped payload = %v", p)
	}
	snap, _ := tk.SnapshotFor(testRepo)
	if lo := snap.Schedules[0].LastOutcome; lo == nil || lo.Kind != OutcomeAlreadyStarted || lo.RunID != "run-prior" {
		t.Errorf("snapshot last outcome = %+v", lo)
	}
}

func TestTick_Refused_CarriesCodeAndMessageVerbatim(t *testing.T) {
	h := newHarness()
	const msg = "workflow upkeep exhausted its weekly budget (limit $1.00, spent $3.00)"
	h.starter.scripted = append(h.starter.scripted, func(StartRequest) (StartOutcome, error) {
		return StartOutcome{Kind: OutcomeRefused, Code: "budget_exhausted", Message: msg, Status: 402}, nil
	})
	tk := h.ticker(run.RunnerKindGitHubActions)
	ctx := context.Background()
	tk.Tick(ctx)
	h.clock.Set(w1.Add(50 * time.Minute))
	tk.Tick(ctx) // a refusal is definitive for the window: no re-attempt

	if got := h.starter.callCount(); got != 1 {
		t.Errorf("starter calls = %d, want 1 (a refused window is not retried)", got)
	}
	if got := h.audit.categories(); !equalStrings(got, []string{CategoryScheduledRunRefused}) {
		t.Fatalf("audit = %v, want [scheduled_run_refused]", got)
	}
	p := h.audit.payload(t, 0)
	if p["code"] != "budget_exhausted" || p["message"] != msg || p["status"] != float64(402) {
		t.Errorf("refused payload = %v", p)
	}
	if _, ok := p["run_id"]; ok {
		t.Errorf("refused payload carries a run_id: %v", p)
	}
	if h.starter.runCount() != 0 {
		t.Errorf("a refused window minted a run")
	}
}

// TestTick_TransientError_NoAuditAndRetriedNextTick pins the transient
// branch: a starter error audits nothing and the SAME window is retried.
//
// COUNTERFACTUAL (approval condition 6): delete the transient branch's
// "do not mark attempted" behaviour (e.g. call t.markAttempted before its
// return) and the retry assertion goes RED: starter calls = 1, not 2, and
// no scheduled_run_started entry. The fixture state that makes the deletion
// observable: ONE ticker (one in-memory attempted map) ticking twice inside
// the SAME window W1, with the starter scripted to fail the first call and
// succeed the second — so the attempted mark is the only thing deciding
// whether the second tick calls the starter at all.
func TestTick_TransientError_NoAuditAndRetriedNextTick(t *testing.T) {
	h := newHarness()
	h.starter.scripted = append(h.starter.scripted, func(StartRequest) (StartOutcome, error) {
		return StartOutcome{}, errors.New("500 internal: database unavailable")
	})
	tk := h.ticker(run.RunnerKindGitHubActions)
	ctx := context.Background()

	tk.Tick(ctx)
	if got := h.audit.categories(); len(got) != 0 {
		t.Fatalf("a transient error audited %v, want nothing", got)
	}
	snap, _ := tk.SnapshotFor(testRepo)
	if lo := snap.Schedules[0].LastOutcome; lo == nil || lo.Kind != OutcomeTransientError || !strings.Contains(lo.Message, "database unavailable") {
		t.Errorf("snapshot last outcome after transient = %+v", lo)
	}

	h.clock.Set(w1.Add(6 * time.Minute)) // same window
	tk.Tick(ctx)
	if got := h.starter.callCount(); got != 2 {
		t.Fatalf("starter calls = %d, want 2 (the transient window is retried)", got)
	}
	if got := h.audit.categories(); !equalStrings(got, []string{CategoryScheduledRunStarted}) {
		t.Errorf("audit after retry = %v, want [scheduled_run_started]", got)
	}
}

// TestTick_UnknownOutcomeKind_RetriedNotAudited pins the starter-contract
// guard: an outcome kind the scheduler does not know is treated like a
// transient failure — not audited, not marked, retried next tick.
func TestTick_UnknownOutcomeKind_RetriedNotAudited(t *testing.T) {
	h := newHarness()
	h.starter.scripted = append(h.starter.scripted, func(StartRequest) (StartOutcome, error) {
		return StartOutcome{Kind: "bogus"}, nil
	})
	tk := h.ticker(run.RunnerKindGitHubActions)
	ctx := context.Background()
	tk.Tick(ctx)
	if got := h.audit.categories(); len(got) != 0 {
		t.Fatalf("an unknown outcome kind audited %v", got)
	}
	tk.Tick(ctx)
	if got := h.starter.callCount(); got != 2 {
		t.Errorf("starter calls = %d, want 2 (an unknown kind is retried)", got)
	}
}

func TestTick_NoPerTickFlood(t *testing.T) {
	h := newHarness()
	tk := h.ticker(run.RunnerKindGitHubActions)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		h.clock.Set(w1.Add(time.Duration(i*5) * time.Minute))
		tk.Tick(ctx)
	}
	if got := h.starter.callCount(); got != 1 {
		t.Errorf("starter calls over ten ticks in one window = %d, want 1", got)
	}
	if got := h.audit.categories(); len(got) != 1 {
		t.Errorf("audit entries over ten ticks in one window = %v, want exactly one", got)
	}
}

func TestTick_SpecFetchError_RecordsSpecErrorNoAudit(t *testing.T) {
	h := newHarness()
	h.specs.err[testRepo] = errors.New("github: 502 bad gateway")
	tk := h.ticker(run.RunnerKindGitHubActions)
	tk.Tick(context.Background())

	if h.starter.callCount() != 0 || len(h.audit.categories()) != 0 {
		t.Fatalf("a fetch error started %d / audited %v, want nothing", h.starter.callCount(), h.audit.categories())
	}
	snap, ok := tk.SnapshotFor(testRepo)
	if !ok || !snap.Scanned || !strings.Contains(snap.SpecError, "502 bad gateway") {
		t.Errorf("snapshot = %+v, want scanned with spec_error", snap)
	}

	// Recovery clears spec_error.
	delete(h.specs.err, testRepo)
	tk.Tick(context.Background())
	snap, _ = tk.SnapshotFor(testRepo)
	if snap.SpecError != "" || len(snap.Schedules) != 1 {
		t.Errorf("snapshot after recovery = %+v, want no spec_error and one schedule", snap)
	}
}

func TestTick_SpecParseError_RecordsSpecErrorNoAudit(t *testing.T) {
	h := newHarness()
	h.specs.content[testRepo] = []byte("version: \"2\"\nworkflows: [not, a, map]\n")
	tk := h.ticker(run.RunnerKindGitHubActions)
	tk.Tick(context.Background())

	if h.starter.callCount() != 0 || len(h.audit.categories()) != 0 {
		t.Fatalf("a parse error started %d / audited %v, want nothing", h.starter.callCount(), h.audit.categories())
	}
	if snap, _ := tk.SnapshotFor(testRepo); snap.SpecError == "" {
		t.Errorf("snapshot spec_error empty after a parse failure: %+v", snap)
	}
}

// TestTick_LatestWindowOnlyAfterGap pins the catch-up policy: after a
// five-window gap only the LATEST window is attempted — no backfill.
func TestTick_LatestWindowOnlyAfterGap(t *testing.T) {
	h := newHarness()
	tk := h.ticker(run.RunnerKindGitHubActions)
	ctx := context.Background()
	tk.Tick(ctx) // W1
	h.clock.Set(w1.Add(5*time.Hour + 20*time.Minute))
	tk.Tick(ctx)

	if got := h.starter.callCount(); got != 2 {
		t.Fatalf("starter calls = %d, want 2 (W1, then only the latest window)", got)
	}
	if got, want := h.starter.calls[1].IdempotencyKey, "scheduled:upkeep:2026-10-02T15:00:00Z"; got != want {
		t.Errorf("post-gap key = %q, want %q (the latest window)", got, want)
	}
}

func TestTick_LocalRunnerCarriesDispatchNote(t *testing.T) {
	h := newHarness()
	tk := h.ticker(run.RunnerKindLocal)
	tk.Tick(context.Background())

	p := h.audit.payload(t, 0)
	if p["dispatch_note"] != DispatchNoteLocal || p["runner_kind"] != run.RunnerKindLocal {
		t.Errorf("local started payload = %v, want the awaiting_host_dispatch note", p)
	}
	if !strings.Contains(DispatchNoteLocal, "awaiting_host_dispatch") {
		t.Errorf("DispatchNoteLocal does not name awaiting_host_dispatch: %q", DispatchNoteLocal)
	}
	snap, _ := tk.SnapshotFor(testRepo)
	if snap.DispatchNote != DispatchNoteLocal || snap.RunnerKind != run.RunnerKindLocal {
		t.Errorf("local snapshot = %+v", snap)
	}
	if h.starter.calls[0].RunnerKind != run.RunnerKindLocal {
		t.Errorf("start request runner_kind = %q, want local", h.starter.calls[0].RunnerKind)
	}
}

func TestTick_UnscheduledWorkflowNeverStarted(t *testing.T) {
	h := newHarness()
	tk := h.ticker(run.RunnerKindGitHubActions)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		h.clock.Set(w1.Add(time.Duration(i) * time.Hour))
		tk.Tick(ctx)
	}
	for _, c := range h.starter.calls {
		if c.WorkflowID != "upkeep" {
			t.Errorf("scheduler started unscheduled workflow %q", c.WorkflowID)
		}
	}
	snap, _ := tk.SnapshotFor(testRepo)
	if len(snap.Schedules) != 1 || snap.Schedules[0].WorkflowID != "upkeep" {
		t.Errorf("snapshot schedules = %+v, want only upkeep", snap.Schedules)
	}
}

// TestTick_AuditAppendFailure_WindowStillDecided pins the best-effort emit:
// an append failure does not un-decide the window (re-attempting would only
// replay the same run).
func TestTick_AuditAppendFailure_WindowStillDecided(t *testing.T) {
	h := newHarness()
	h.audit.err = errors.New("audit: connection refused")
	tk := h.ticker(run.RunnerKindGitHubActions)
	ctx := context.Background()
	tk.Tick(ctx)
	tk.Tick(ctx)
	if got := h.starter.callCount(); got != 1 {
		t.Errorf("starter calls = %d, want 1 (an audit failure does not re-open the window)", got)
	}
}

func TestTick_MissingDependency_NoOp(t *testing.T) {
	cases := map[string]func(*Ticker){
		"nil specs":   func(tk *Ticker) { tk.Specs = nil },
		"nil starter": func(tk *Ticker) { tk.Starter = nil },
		"nil audit":   func(tk *Ticker) { tk.Audit = nil },
		"no repos":    func(tk *Ticker) { tk.Repos = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			tk := h.ticker(run.RunnerKindGitHubActions)
			mutate(tk)
			tk.Tick(context.Background())
			if h.specs.calls != 0 || h.starter.callCount() != 0 || len(h.audit.categories()) != 0 {
				t.Errorf("tick with %s did work: fetches=%d starts=%d audit=%v",
					name, h.specs.calls, h.starter.callCount(), h.audit.categories())
			}
			if err := tk.Run(context.Background()); !errors.Is(err, errMissingDeps) {
				t.Errorf("Run with %s = %v, want errMissingDeps", name, err)
			}
		})
	}
}

func TestRun_TicksAtStartupAndStopsOnCancel(t *testing.T) {
	h := newHarness()
	tk := h.ticker(run.RunnerKindGitHubActions)
	tk.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tk.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for h.starter.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not tick at startup")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSnapshot_States(t *testing.T) {
	h := newHarness()
	tk := h.ticker(run.RunnerKindGitHubActions)

	if _, ok := tk.SnapshotFor("someone/else"); ok {
		t.Error("SnapshotFor an unconfigured repo reported ok")
	}
	snap, ok := tk.SnapshotFor(testRepo)
	if !ok || snap.Scanned || len(snap.Schedules) != 0 || snap.DispatchNote != "" {
		t.Errorf("pre-tick snapshot = %+v, want configured, unscanned, empty", snap)
	}

	tk.Tick(context.Background())
	all := tk.Snapshot()
	if len(all) != 1 {
		t.Fatalf("Snapshot() len = %d, want 1", len(all))
	}
	s := all[0]
	if !s.Scanned || !s.LastTickAt.Equal(w1.Add(5*time.Minute)) || len(s.Schedules) != 1 {
		t.Fatalf("post-tick snapshot = %+v", s)
	}
	w := s.Schedules[0]
	if w.Cron != "0 * * * *" || w.Timezone != spec.DefaultScheduleTimezone || w.Issue != 3725 ||
		!w.CurrentWindowStart.Equal(w1) || !w.NextDueAt.Equal(w2) {
		t.Errorf("workflow snapshot = %+v", w)
	}
	if w.LastOutcome == nil || w.LastOutcome.Kind != OutcomeStarted || w.LastOutcome.RunID != "run-1" || !w.LastOutcome.WindowStart.Equal(w1) {
		t.Errorf("last outcome = %+v", w.LastOutcome)
	}

	// A schedule removed from the spec drops out of the snapshot.
	h.specs.content[testRepo] = []byte(strings.Replace(string(hourlySpec), "    schedule:\n      cron: \"0 * * * *\"\n      issue: 3725\n", "", 1))
	tk.Tick(context.Background())
	if snap, _ := tk.SnapshotFor(testRepo); len(snap.Schedules) != 0 {
		t.Errorf("snapshot after schedule removal = %+v, want no schedules", snap.Schedules)
	}
}

// TestTickWorkflow_UnparseableSchedule_RecordedNotStarted drives the
// defence-in-depth branch directly: ParseBytes' validation refuses such a
// spec before tickRepo reaches it, so only an internal call can construct it.
func TestTickWorkflow_UnparseableSchedule_RecordedNotStarted(t *testing.T) {
	h := newHarness()
	tk := h.ticker(run.RunnerKindGitHubActions)
	tk.tickWorkflow(context.Background(), tk.logger(), testRepo, "broken",
		spec.Schedule{Cron: "61 * * * *"}, hourlySpec, "sha", h.clock.Now())

	if h.starter.callCount() != 0 || len(h.audit.categories()) != 0 {
		t.Fatalf("an unparseable schedule started %d / audited %v", h.starter.callCount(), h.audit.categories())
	}
	snap, _ := tk.SnapshotFor(testRepo)
	if len(snap.Schedules) != 1 || snap.Schedules[0].ScheduleError == "" {
		t.Errorf("snapshot = %+v, want the schedule error recorded", snap.Schedules)
	}
}
