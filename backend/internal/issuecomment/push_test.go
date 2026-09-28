package issuecomment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pushnotify"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// ---------------------------------------------------------------------
// Fakes. Every fake guards its bookkeeping with a mutex: the concurrency
// test drives 8 concurrent notifier calls, and a racy fake would make a
// -race RED attributable to the FAKE rather than the claim lock (#2586).
// ---------------------------------------------------------------------

type pushFakeRuns struct {
	run.Repository
	mu     sync.Mutex
	runs   map[uuid.UUID]*run.Run
	stages map[uuid.UUID][]*run.Stage
}

func (f *pushFakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	return r, nil
}

func (f *pushFakeRuns) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stages[id], nil
}

type pushFakeAudit struct {
	audit.Repository
	mu        sync.Mutex
	entries   []*audit.Entry
	failOnCat string
}

func (f *pushFakeAudit) seed(runID uuid.UUID, stageID *uuid.UUID, category string, ts time.Time, payload any) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(payload)
	r := runID
	e := &audit.Entry{ID: uuid.New(), Sequence: int64(len(f.entries) + 1), RunID: &r, StageID: stageID,
		Category: category, Timestamp: ts, Payload: body}
	f.entries = append(f.entries, e)
	return e.Sequence
}

func (f *pushFakeAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCat != "" && p.Category == f.failOnCat {
		return nil, errors.New("fake audit: append refused")
	}
	r := p.RunID
	e := &audit.Entry{ID: uuid.New(), Sequence: int64(len(f.entries) + 1), RunID: &r, StageID: p.StageID,
		Category: p.Category, Timestamp: p.Timestamp, Payload: p.Payload}
	f.entries = append(f.entries, e)
	return e, nil
}

func (f *pushFakeAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*audit.Entry
	for _, e := range f.entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *pushFakeAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	all, _ := f.ListForRun(ctx, runID)
	var out []*audit.Entry
	for _, e := range all {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

// recordingSink counts deliveries and records each Event + the ctx error
// observed at delivery. gate, when non-nil, blocks Deliver until closed.
type recordingSink struct {
	name   string
	gate   chan struct{}
	err    error
	mu     sync.Mutex
	events []pushnotify.Event
	ctxErr []error
	// entered is signalled (non-blocking) when Deliver starts.
	entered chan struct{}
}

func (s *recordingSink) Name() string { return s.name }

func (s *recordingSink) Deliver(ctx context.Context, e pushnotify.Event) error {
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	s.events = append(s.events, e)
	s.ctxErr = append(s.ctxErr, ctx.Err())
	s.mu.Unlock()
	return s.err
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func (s *recordingSink) snapshot() ([]pushnotify.Event, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pushnotify.Event(nil), s.events...), append([]error(nil), s.ctxErr...)
}

var pushT0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

type pushHarness struct {
	runID uuid.UUID
	runs  *pushFakeRuns
	au    *pushFakeAudit
	disp  *pushnotify.Dispatcher
	ch    *PushChannel
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newPushHarness wires a PushChannel over fakes and a REAL dispatcher. The
// run is issue-anchored unless r overrides it.
func newPushHarness(t *testing.T, opts pushnotify.Options, sinks ...pushnotify.Sink) *pushHarness {
	t.Helper()
	h := &pushHarness{runID: uuid.New(), au: &pushFakeAudit{}, logs: &syncBuffer{}}
	ref := "issue:42"
	h.runs = &pushFakeRuns{
		runs: map[uuid.UUID]*run.Run{h.runID: {
			ID: h.runID, Repo: "acme/widgets", WorkflowID: "feature_change",
			TriggerSource: run.TriggerGitHubIssue, TriggerRef: &ref, CreatedAt: pushT0,
		}},
		stages: map[uuid.UUID][]*run.Stage{},
	}
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opts.Logger = log
	if opts.OnOutcome == nil {
		opts.OnOutcome = PushOutcomeRecorder(h.au, func() time.Time { return pushT0 }, log)
	}
	h.disp = pushnotify.NewDispatcher(sinks, opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timescale.D(5*time.Second))
		defer cancel()
		_ = h.disp.Close(ctx)
	})
	h.ch = NewPushChannel(PushDeps{
		Runs: h.runs, Audit: h.au, ExternalURL: "https://fishhawk.example.com",
		Dispatcher: h.disp, Now: func() time.Time { return pushT0 }, Logger: log,
	})
	if h.ch == nil {
		t.Fatal("NewPushChannel returned nil")
	}
	return h
}

func (h *pushHarness) drain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(10*time.Second))
	defer cancel()
	if err := h.disp.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func (h *pushHarness) rows(t *testing.T, category string) []map[string]any {
	t.Helper()
	es, _ := h.au.ListForRunByCategory(context.Background(), h.runID, category)
	out := make([]map[string]any, 0, len(es))
	for _, e := range es {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		out = append(out, m)
	}
	return out
}

func (h *pushHarness) seedScopeAmendment() int64 {
	return h.au.seed(h.runID, nil, "scope_amendment_requested", pushT0.Add(time.Minute), map[string]any{})
}

// ---------------------------------------------------------------------
// End-to-end: pgtest-backed chain -> projection -> claim -> queue ->
// worker -> REAL webhook sink -> HTTP wire.
// ---------------------------------------------------------------------

type capturedDelivery struct {
	body    []byte
	headers http.Header
}

func TestPushChannel_EndToEndWebhookDelivery(t *testing.T) {
	pool := pgtest.NewPool(t)
	runs := run.NewPostgresRepository(pool)
	au := audit.NewPostgresRepository(pool)
	ctx := context.Background()

	var mu sync.Mutex
	var got []capturedDelivery
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, capturedDelivery{body: b, headers: r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	const secret = "e2e-shared-secret"
	sinks, err := pushnotify.SinksFromEnv(envMap(map[string]string{
		pushnotify.EnvWebhookURL:    srv.URL + "/hook",
		pushnotify.EnvWebhookSecret: secret,
	}))
	if err != nil || len(sinks) != 1 {
		t.Fatalf("SinksFromEnv = %v, %v", sinks, err)
	}
	disp := pushnotify.NewDispatcher(sinks, pushnotify.Options{OnOutcome: PushOutcomeRecorder(au, nil, nil)})
	t.Cleanup(func() { _ = disp.Close(context.Background()) })
	ch := NewPushChannel(PushDeps{Runs: runs, Audit: au, ExternalURL: "https://fishhawk.example.com", Dispatcher: disp})

	// mkRun creates a run with a plan stage; parked moves the stage to
	// awaiting_approval and seeds a two-round plan chain so gate latency is
	// non-trivial (8m on the first round's approval gate).
	mkRun := func(t *testing.T, source run.TriggerSource, ref *string, parked bool) (*run.Run, *run.Stage, int64) {
		t.Helper()
		r, err := runs.CreateRun(ctx, run.CreateRunParams{Repo: "acme/widgets", WorkflowID: "feature_change",
			WorkflowSHA: "deadbeef", TriggerSource: source, TriggerRef: ref})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		st, err := runs.CreateStage(ctx, run.CreateStageParams{RunID: r.ID, Type: run.StageTypePlan,
			ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code"})
		if err != nil {
			t.Fatalf("create stage: %v", err)
		}
		states := []run.StageState{run.StageStateDispatched, run.StageStateRunning}
		if parked {
			states = append(states, run.StageStateAwaitingApproval)
		}
		for _, s := range states {
			if _, err := runs.TransitionStage(ctx, st.ID, s, nil); err != nil {
				t.Fatalf("transition %s: %v", s, err)
			}
		}
		sys := audit.ActorSystem
		app := func(cat string, at time.Duration, payload any) int64 {
			b, _ := json.Marshal(payload)
			sid := st.ID
			e, err := au.AppendChained(ctx, audit.ChainAppendParams{RunID: r.ID, StageID: &sid,
				Timestamp: r.CreatedAt.Add(at), Category: cat, ActorKind: &sys, Payload: b})
			if err != nil {
				t.Fatalf("append %s: %v", cat, err)
			}
			return e.Sequence
		}
		app("plan_generated", time.Minute, map[string]any{})
		app("approval_submitted", 9*time.Minute, map[string]any{"decision": "reject"})
		parkSeq := app("plan_generated", 10*time.Minute, map[string]any{})
		app("plan_reviewed", 11*time.Minute, map[string]any{"verdict": "approve", "reviewer_model": "model-a"})
		app("plan_reviewed", 12*time.Minute, map[string]any{"verdict": "approve", "reviewer_model": "model-b"})
		return r, st, parkSeq
	}

	decode := func(t *testing.T, b []byte) pushnotify.Event {
		t.Helper()
		var e pushnotify.Event
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("decode delivered body: %v\n%s", err, b)
		}
		return e
	}
	take := func() []capturedDelivery {
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
	drain := func(t *testing.T) {
		t.Helper()
		dctx, cancel := context.WithTimeout(ctx, timescale.D(10*time.Second))
		defer cancel()
		if err := disp.Drain(dctx); err != nil {
			t.Fatalf("drain: %v", err)
		}
	}

	t.Run("issue_anchored", func(t *testing.T) {
		ref := "issue:42"
		r, st, parkSeq := mkRun(t, run.TriggerGitHubIssue, &ref, true)
		if err := ch.NotifyPageClassForRun(ctx, r.ID); err != nil {
			t.Fatalf("notify: %v", err)
		}
		drain(t)
		ds := take()
		if len(ds) != 1 {
			t.Fatalf("deliveries = %d, want 1", len(ds))
		}
		e := decode(t, ds[0].body)
		if e.RunID != r.ID.String() || e.Repo != "acme/widgets" || e.SourceSequence != parkSeq ||
			e.Event != "plan_awaiting_approval" {
			t.Errorf("identity fields wrong: %+v", e)
		}
		if e.Issue == nil || e.Issue.Number != 42 || e.Links.Issue != "https://github.com/acme/widgets/issues/42" {
			t.Errorf("issue block wrong: %+v links=%+v", e.Issue, e.Links)
		}
		if e.Decision != "A plan is ready and awaiting your review" {
			t.Errorf("decision = %q", e.Decision)
		}
		if len(e.Verdicts) != 2 || e.Verdicts[0].ReviewerModel != "model-a" || e.Verdicts[1].Verdict != "approve" {
			t.Errorf("verdicts = %+v", e.Verdicts)
		}
		if e.Stage == nil || e.Stage.Type != string(run.StageTypePlan) || e.Stage.State != string(run.StageStateAwaitingApproval) {
			t.Errorf("stage = %+v (stage id %s)", e.Stage, st.ID)
		}
		// Gate latency is the BuildRunEconomics fold over the SAME chain.
		entries, _ := au.ListForRun(ctx, r.ID)
		want := pushGateLatency(BuildRunEconomics(r, entries, nil))
		if want.TotalWaitOnHumanSeconds < 8*60 {
			t.Fatalf("fixture produced no gate latency: %+v", want)
		}
		if e.GateLatency.TotalWaitOnHumanSeconds != want.TotalWaitOnHumanSeconds || len(e.GateLatency.Gates) != len(want.Gates) {
			t.Errorf("gate_latency = %+v, want %+v", e.GateLatency, want)
		}
		if e.Links.Run != "https://fishhawk.example.com/runs/"+r.ID.String() {
			t.Errorf("run link = %q", e.Links.Run)
		}
		if h := ds[0].headers.Get(pushnotify.HeaderDelivery); h != fmt.Sprintf("%s:%d", r.ID, parkSeq) {
			t.Errorf("X-Fishhawk-Delivery = %q", h)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(ds[0].body)
		if h := ds[0].headers.Get(pushnotify.HeaderSignature); h != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Errorf("signature header does not verify over the received body")
		}
		claims, _ := au.ListForRunByCategory(ctx, r.ID, CategoryPushNotificationSent)
		if len(claims) != 1 {
			t.Fatalf("committed claim rows = %d, want 1", len(claims))
		}
		var c struct {
			SourceSequence int64    `json:"source_sequence"`
			Sinks          []string `json:"sinks"`
		}
		_ = json.Unmarshal(claims[0].Payload, &c)
		if c.SourceSequence != parkSeq || len(c.Sinks) != 1 || c.Sinks[0] != "webhook" {
			t.Errorf("claim payload = %s", claims[0].Payload)
		}
		// A second notify re-reads the committed claim and sends nothing.
		_ = ch.NotifyStatusUpdateForRun(ctx, r.ID)
		drain(t)
		if n := len(take()); n != 0 {
			t.Errorf("re-notify delivered %d, want 0", n)
		}
	})

	t.Run("cli_triggered_not_issue_anchored", func(t *testing.T) {
		r, _, parkSeq := mkRun(t, run.TriggerCLI, nil, true)
		if err := ch.NotifyPageClassForRun(ctx, r.ID); err != nil {
			t.Fatalf("notify: %v", err)
		}
		drain(t)
		ds := take()
		if len(ds) != 1 {
			t.Fatalf("CLI-triggered run: deliveries = %d, want 1 (push must fire for every run)", len(ds))
		}
		e := decode(t, ds[0].body)
		if e.Issue != nil || e.Links.Issue != "" {
			t.Errorf("CLI run carried an issue block/link: %+v %+v", e.Issue, e.Links)
		}
		if e.SourceSequence != parkSeq || bytes.Contains(ds[0].body, []byte(`"issue"`)) {
			t.Errorf("CLI payload wrong: %s", ds[0].body)
		}
	})

	t.Run("no_parked_plan_delivers_nothing", func(t *testing.T) {
		ref := "issue:43"
		r, _, _ := mkRun(t, run.TriggerGitHubIssue, &ref, false)
		_ = ch.NotifyPageClassForRun(ctx, r.ID)
		drain(t)
		if n := len(take()); n != 0 {
			t.Errorf("unparked run delivered %d, want 0", n)
		}
	})
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// ---------------------------------------------------------------------
// Async / request-path (constraint 1).
// ---------------------------------------------------------------------

// A1: the notifier returns promptly while a sink is still blocked, and the
// claim has already landed.
func TestPushChannel_DoesNotBlockOnSlowSink(t *testing.T) {
	gate := make(chan struct{})
	sink := &recordingSink{name: "slow", gate: gate}
	h := newPushHarness(t, pushnotify.Options{SinkTimeout: time.Hour}, sink)
	t.Cleanup(func() { close(gate) })
	h.seedScopeAmendment()

	bound := timescale.D(500 * time.Millisecond)
	start := time.Now()
	if err := h.ch.NotifyPageClassForRun(context.Background(), h.runID); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if el := time.Since(start); el > bound {
		t.Fatalf("NotifyPageClassForRun took %v while the sink was blocked (bound %v): delivery is on the request path", el, bound)
	}
	if n := len(h.rows(t, CategoryPushNotificationSent)); n != 1 {
		t.Errorf("claim rows = %d, want 1", n)
	}
}

// A2: the REQUEST context is cancelled while the sink is blocked; at
// release the sink's ctx must still be live.
func TestDispatcher_DeliverySurvivesCallerContextCancellation(t *testing.T) {
	gate := make(chan struct{})
	sink := &recordingSink{name: "rec", gate: gate, entered: make(chan struct{}, 1)}
	h := newPushHarness(t, pushnotify.Options{SinkTimeout: timescale.D(10 * time.Second)}, sink)
	h.seedScopeAmendment()

	reqCtx, cancel := context.WithCancel(context.Background())
	if err := h.ch.NotifyPageClassForRun(reqCtx, h.runID); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case <-sink.entered:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("sink never entered Deliver")
	}
	cancel()
	close(gate)
	h.drain(t)
	evs, errs := sink.snapshot()
	if len(evs) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(evs))
	}
	if errs[0] != nil {
		t.Errorf("sink ctx.Err() at release = %v, want nil (worker ctx derived from the request ctx)", errs[0])
	}
}

// A4: a full queue drops without blocking and records a queue_full row.
func TestDispatcher_FullQueueDropsWithFailureRow(t *testing.T) {
	gate := make(chan struct{})
	sink := &recordingSink{name: "gated", gate: gate, entered: make(chan struct{}, 1)}
	h := newPushHarness(t, pushnotify.Options{QueueSize: 1, Workers: 1, SinkTimeout: time.Hour}, sink)
	t.Cleanup(func() { close(gate) })
	// First event occupies the worker.
	h.seedScopeAmendment()
	if err := h.ch.NotifyPageClassForRun(context.Background(), h.runID); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case <-sink.entered:
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("worker never picked up the first event")
	}
	// Second fills the queue (size 1); third must drop.
	h.seedScopeAmendment()
	h.seedScopeAmendment()
	done := make(chan error, 1)
	go func() { done <- h.ch.NotifyPageClassForRun(context.Background(), h.runID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("notify returned %v, want nil", err)
		}
	case <-time.After(timescale.D(2 * time.Second)):
		t.Fatal("NotifyPageClassForRun blocked on a full queue")
	}
	fails := h.rows(t, CategoryPushNotificationFailed)
	if len(fails) != 1 || fails[0]["sink"] != "*" || fails[0]["error"] != pushQueueFullReason {
		t.Fatalf("failure rows = %v, want one queue_full row with sink \"*\"", fails)
	}
	if !strings.Contains(h.logs.String(), "queue full") {
		t.Errorf("no queue-full WARN logged")
	}
}

// ---------------------------------------------------------------------
// At-most-once (constraint 2 / binding condition 3).
// ---------------------------------------------------------------------

// C1: the hook between the dedup read and the claim append waits until N
// callers arrived OR a scaled timeout. With the claim lock callers arrive
// one at a time, so only the first sends; without it all N arrive together,
// all read "unclaimed", and delivery count > 1.
func TestPushChannel_ConcurrentNotifiesDeliverExactlyOnce(t *testing.T) {
	sink := &recordingSink{name: "count"}
	h := newPushHarness(t, pushnotify.Options{}, sink)
	h.seedScopeAmendment()

	const n = 8
	wait := timescale.D(50 * time.Millisecond)
	var amu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	h.ch.afterDedupRead = func(uuid.UUID) {
		amu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		amu.Unlock()
		select {
		case <-all:
		case <-time.After(wait):
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.ch.NotifyPageClassForRun(context.Background(), h.runID)
		}()
	}
	wg.Wait()
	h.drain(t)
	if c := sink.count(); c != 1 {
		t.Errorf("deliveries = %d, want exactly 1", c)
	}
	if rows := h.rows(t, CategoryPushNotificationSent); len(rows) != 1 {
		t.Errorf("committed claim rows = %d, want exactly 1", len(rows))
	}
}

// C2: a failed claim append sends NOTHING and WARNs.
func TestPushChannel_ClaimBeforeDispatch(t *testing.T) {
	sink := &recordingSink{name: "count"}
	h := newPushHarness(t, pushnotify.Options{}, sink)
	h.au.failOnCat = CategoryPushNotificationSent
	h.seedScopeAmendment()
	if err := h.ch.NotifyPageClassForRun(context.Background(), h.runID); err != nil {
		t.Fatalf("notify returned %v, want nil", err)
	}
	h.drain(t)
	if c := sink.count(); c != 0 {
		t.Errorf("deliveries = %d after a failed claim, want 0", c)
	}
	if !strings.Contains(h.logs.String(), "claim append failed") {
		t.Errorf("no claim-failure WARN logged:\n%s", h.logs.String())
	}
}

// C3: an event already carrying a committed claim row never re-pushes.
func TestPushChannel_DoesNotRePushSamePageEvent(t *testing.T) {
	sink := &recordingSink{name: "count"}
	h := newPushHarness(t, pushnotify.Options{}, sink)
	seq := h.seedScopeAmendment()
	h.au.seed(h.runID, nil, CategoryPushNotificationSent, pushT0, map[string]any{"source_sequence": seq, "event": "scope_amendment"})
	for i := 0; i < 2; i++ {
		_ = h.ch.NotifyPageClassForRun(context.Background(), h.runID)
	}
	h.drain(t)
	if c := sink.count(); c != 0 {
		t.Errorf("deliveries = %d, want 0 for an already-claimed event", c)
	}
	if rows := h.rows(t, CategoryPushNotificationSent); len(rows) != 1 {
		t.Errorf("claim rows = %d, want exactly 1", len(rows))
	}
}

// C4 (AC7): a fixup re-park does not re-push the old reject; a genuinely
// new source sequence pushes once.
func TestPushChannel_FixupRepark_DoesNotRePush(t *testing.T) {
	sink := &recordingSink{name: "count"}
	h := newPushHarness(t, pushnotify.Options{}, sink)
	rej := h.au.seed(h.runID, nil, "implement_reviewed", pushT0, map[string]any{"verdict": "reject", "reviewer_model": "model-x"})
	_ = h.ch.NotifyPageClassForRun(context.Background(), h.runID)
	h.drain(t)
	if c := sink.count(); c != 1 {
		t.Fatalf("first reject deliveries = %d, want 1", c)
	}
	h.au.seed(h.runID, nil, "stage_fixup_triggered", pushT0.Add(time.Minute), map[string]any{})
	_ = h.ch.NotifyStatusUpdateForRun(context.Background(), h.runID)
	h.drain(t)
	if c := sink.count(); c != 1 {
		t.Fatalf("after fixup re-park deliveries = %d, want still 1", c)
	}
	rej2 := h.au.seed(h.runID, nil, "implement_reviewed", pushT0.Add(2*time.Minute), map[string]any{"verdict": "reject", "reviewer_model": "model-y"})
	_ = h.ch.NotifyPageClassForRun(context.Background(), h.runID)
	h.drain(t)
	evs, _ := sink.snapshot()
	if len(evs) != 2 || evs[0].SourceSequence != rej || evs[1].SourceSequence != rej2 {
		t.Fatalf("deliveries = %+v, want exactly [%d, %d]", evs, rej, rej2)
	}
	if len(evs[1].Verdicts) != 1 || evs[1].Verdicts[0].ReviewerModel != "model-y" {
		t.Errorf("second-round verdicts = %+v, want only model-y", evs[1].Verdicts)
	}
}

// A stale (already-arbitrated) reviewer reject is claimed but never sent.
func TestPushChannel_ResolvedRejectClaimedNotSent(t *testing.T) {
	sink := &recordingSink{name: "count"}
	h := newPushHarness(t, pushnotify.Options{}, sink)
	h.au.seed(h.runID, nil, "plan_reviewed", pushT0, map[string]any{"verdict": "reject"})
	h.au.seed(h.runID, nil, "approval_submitted", pushT0.Add(time.Minute), map[string]any{"decision": "approve"})
	_ = h.ch.NotifyPageClassForRun(context.Background(), h.runID)
	h.drain(t)
	if c := sink.count(); c != 0 {
		t.Errorf("deliveries = %d, want 0 for a resolved reject", c)
	}
	rows := h.rows(t, CategoryPushNotificationSent)
	if len(rows) != 1 || rows[0]["suppressed"] != "resolved" {
		t.Errorf("claim rows = %v, want one suppressed=resolved row", rows)
	}
}

// ---------------------------------------------------------------------
// Outcome recording (constraint 3).
// ---------------------------------------------------------------------

// O1: a failing sink records a separate failure row; the notifier still
// returns nil and the claim row names the target sinks.
func TestPushChannel_SinkFailureRecordsFailureRow(t *testing.T) {
	failing := &recordingSink{name: "webhook", err: &pushnotify.DeliveryError{Sink: "webhook", Host: "hooks.example", Reason: "http status 500"}}
	h := newPushHarness(t, pushnotify.Options{}, failing)
	seq := h.seedScopeAmendment()
	if err := h.ch.NotifyPageClassForRun(context.Background(), h.runID); err != nil {
		t.Fatalf("notify returned %v, want nil", err)
	}
	h.drain(t)
	claims := h.rows(t, CategoryPushNotificationSent)
	if len(claims) != 1 || claims[0]["event"] != "scope_amendment" || claims[0]["source_sequence"] != float64(seq) {
		t.Fatalf("claim rows = %v", claims)
	}
	if sinks, _ := claims[0]["sinks"].([]any); len(sinks) != 1 || sinks[0] != "webhook" {
		t.Errorf("claim sinks = %v, want [webhook]", claims[0]["sinks"])
	}
	fails := h.rows(t, CategoryPushNotificationFailed)
	if len(fails) != 1 || fails[0]["sink"] != "webhook" || fails[0]["source_sequence"] != float64(seq) ||
		fails[0]["error"] != `push sink "webhook": hooks.example: http status 500` {
		t.Fatalf("failure rows = %v", fails)
	}
}

// O2 pair: neither push category is operator-timeline activity.
func TestPushCategoriesAreNotActivity(t *testing.T) {
	for _, c := range []string{CategoryPushNotificationSent, CategoryPushNotificationFailed} {
		if !audit.IsKnownCategory(c) {
			t.Errorf("%q not registered in audit/categories.go", c)
		}
		if _, ok := activityCategories[c]; ok {
			t.Errorf("%q is in activityCategories; push rows are delivery records, not activity", c)
		}
	}
}

// ---------------------------------------------------------------------
// Credential leaks (binding condition 2): canary in URL path, query and
// userinfo, plus the SMTP password; a transport error for every sink.
// ---------------------------------------------------------------------

func TestPushChannel_TransportErrorsLeakNoCredentials(t *testing.T) {
	const (
		canaryPath  = "CANARYPATH0001"
		canaryQuery = "CANARYQUERY0002"
		canaryUser  = "CANARYUSER0003"
		canaryPass  = "CANARYPASS0004"
		canarySMTP  = "CANARYSMTPPW0005"
		canarySec   = "CANARYSECRET0006"
	)
	url := "http://" + canaryUser + ":" + canaryPass + "@127.0.0.1:1/services/" + canaryPath + "?token=" + canaryQuery
	sinks, err := pushnotify.SinksFromEnv(envMap(map[string]string{
		pushnotify.EnvWebhookURL:      url,
		pushnotify.EnvWebhookSecret:   canarySec,
		pushnotify.EnvSlackWebhookURL: url,
		pushnotify.EnvEmailSMTPAddr:   "127.0.0.1:1",
		pushnotify.EnvEmailFrom:       "fishhawk@example.com",
		pushnotify.EnvEmailTo:         "ops@example.com",
		pushnotify.EnvEmailUsername:   "ops",
		pushnotify.EnvEmailPassword:   canarySMTP,
	}))
	if err != nil || len(sinks) != 3 {
		t.Fatalf("SinksFromEnv = %d sinks, %v", len(sinks), err)
	}
	h := newPushHarness(t, pushnotify.Options{SinkTimeout: timescale.D(5 * time.Second)}, sinks...)
	h.seedScopeAmendment()
	if err := h.ch.NotifyPageClassForRun(context.Background(), h.runID); err != nil {
		t.Fatalf("notify: %v", err)
	}
	h.drain(t)
	fails, _ := h.au.ListForRunByCategory(context.Background(), h.runID, CategoryPushNotificationFailed)
	if len(fails) != 3 {
		t.Fatalf("failure rows = %d, want 3 (one per sink)", len(fails))
	}
	var rows strings.Builder
	for _, f := range fails {
		rows.Write(f.Payload)
	}
	logs := h.logs.String()
	for _, name := range []string{"webhook", "slack", "email"} {
		if !strings.Contains(rows.String(), `"sink":"`+name+`"`) || !strings.Contains(logs, name) {
			t.Fatalf("sink %q absent from rows/logs — the leak assertion would pass vacuously\nrows=%s", name, rows.String())
		}
	}
	for _, c := range []string{canaryPath, canaryQuery, canaryUser, canaryPass, canarySMTP, canarySec} {
		if strings.Contains(rows.String(), c) {
			t.Errorf("canary %q leaked into a push_notification_failed row", c)
		}
		if strings.Contains(logs, c) {
			t.Errorf("canary %q leaked into a log record", c)
		}
	}
}

// ---------------------------------------------------------------------
// Documented payload contract.
// ---------------------------------------------------------------------

// TestDocumentedPayloadMatchesEventStruct reads the canonical fenced JSON
// block out of docs/notifications.md (between the push-payload markers) and
// asserts it decodes into pushnotify.Event with DisallowUnknownFields and
// re-marshals to the same field set — so the external contract cannot drift
// from the struct.
func TestDocumentedPayloadMatchesEventStruct(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "notifications.md"))
	if err != nil {
		t.Fatalf("read docs/notifications.md: %v", err)
	}
	const begin, end = "<!-- BEGIN canonical-payload -->", "<!-- END canonical-payload -->"
	s := string(doc)
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		t.Fatalf("docs/notifications.md lacks the %s / %s markers", begin, end)
	}
	block := s[i+len(begin) : j]
	block = block[strings.Index(block, "```json")+len("```json"):]
	block = block[:strings.Index(block, "```")]

	var e pushnotify.Event
	dec := json.NewDecoder(strings.NewReader(block))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("documented payload does not decode into pushnotify.Event: %v", err)
	}
	if e.SourceSequence == 0 {
		t.Error("documented payload omits source_sequence")
	}
	var docFields, structFields map[string]any
	_ = json.Unmarshal([]byte(block), &docFields)
	b, _ := pushnotify.MarshalEvent(e)
	_ = json.Unmarshal(b, &structFields)
	if !sameKeys(docFields, structFields) {
		t.Errorf("field set drift:\n doc    = %v\n struct = %v", keysOf(docFields), keysOf(structFields))
	}
}

func sameKeys(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		am, aok := av.(map[string]any)
		bm, bok := bv.(map[string]any)
		if aok != bok || (aok && !sameKeys(am, bm)) {
			return false
		}
	}
	return true
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
