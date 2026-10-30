package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// ctrRepo is the fixture repository: a non-Fishhawk repository (ADR-110).
const ctrRepo = "acme/widgets"

// ctrRunRepo answers ListRuns by pull request URL; every other method
// panics through the nil embedded interface.
type ctrRunRepo struct {
	run.Repository
	byPR map[string]*run.Run
	err  error
}

func (r ctrRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	if r.err != nil {
		return nil, r.err
	}
	if f.PullRequestURL == nil {
		return nil, nil
	}
	if x, ok := r.byPR[*f.PullRequestURL]; ok {
		return []*run.Run{x}, nil
	}
	return nil, nil
}

// ctrQueue answers ActiveForRun from one status and counts the reads; every
// other method panics through the nil embedded interface.
type ctrQueue struct {
	mergequeue.Store
	mu    sync.Mutex
	st    mergequeue.Status
	found bool
	err   error
	reads int
}

func (q *ctrQueue) ActiveForRun(context.Context, uuid.UUID) (mergequeue.Status, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reads++
	return q.st, q.found, q.err
}

func (q *ctrQueue) readCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.reads
}

// ctrYielded is a held, yielded, intent-bearing entry anchored at head.
func ctrYielded(runID uuid.UUID, head string) mergequeue.Entry {
	at := time.Now()
	return mergequeue.Entry{
		ID: uuid.New(), RunID: runID, Repo: ctrRepo, BaseRef: "main",
		State: mergequeue.StateHeld, AnchoredHeadSHA: head,
		MergeIntentAt: &at, YieldedAt: &at, YieldReason: "merge_checks_red",
	}
}

// ctrPayload is a check_run delivery on PR #7 of ctrRepo.
func ctrPayload(action string, id int64, name, head string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"check_run": map[string]any{
			"id": id, "name": name, "head_sha": head, "status": "completed",
			"conclusion":    "success",
			"pull_requests": []map[string]any{{"number": 7}},
		},
		"repository": map[string]any{"full_name": ctrRepo},
		"sender":     map[string]any{"login": "ci-bot"},
	})
	return b
}

const ctrPRURL = "https://github.com/" + ctrRepo + "/pull/7"

// ctrWorld is a server wired with the fake run repository and queue for one
// run on PR #7.
type ctrWorld struct {
	srv   *Server
	queue *ctrQueue
	run   *run.Run
}

func newCtrWorld(t *testing.T, snap *run.RequiredChecksSnapshot, entry func(runID uuid.UUID) mergequeue.Entry) *ctrWorld {
	t.Helper()
	r := &run.Run{ID: uuid.New(), RequiredChecksSnapshot: snap}
	q := &ctrQueue{found: true}
	q.st = mergequeue.Status{Entry: entry(r.ID)}
	srv := New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
		RunRepo:             ctrRunRepo{byPR: map[string]*run.Run{ctrPRURL: r}},
		MergeQueue:          q,
	})
	return &ctrWorld{srv: srv, queue: q, run: r}
}

// ctrProbes records the probes a test coalescer runs.
type ctrProbes struct {
	mu   sync.Mutex
	runs []uuid.UUID
}

func (p *ctrProbes) probe(_ context.Context, runID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs = append(p.runs, runID)
}

func (p *ctrProbes) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.runs)
}

// TestCheckRunTrigger_Filter pins the A3.8 trigger filter and every skip of
// the webhook consumer, one row per mode: only a completed check at a held,
// yielded, intent-bearing entry's anchored head, whose name is required (or
// any name when the snapshot is nil or empty), fires the probe.
func TestCheckRunTrigger_Filter(t *testing.T) {
	const head, other = "aaa111", "bbb222"
	required := &run.RequiredChecksSnapshot{Contexts: []string{"CI Pass"}, Sources: []string{"branch_protection"}}
	empty := &run.RequiredChecksSnapshot{}
	yielded := func(id uuid.UUID) mergequeue.Entry { return ctrYielded(id, head) }
	cases := []struct {
		name   string
		snap   *run.RequiredChecksSnapshot
		entry  func(uuid.UUID) mergequeue.Entry
		body   []byte
		mutate func(w *ctrWorld)
		want   int
	}{
		{name: "required name triggers", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", head), want: 1},
		{name: "non-required name with a snapshot does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "lint", head)},
		{name: "nil snapshot triggers on any observed name", entry: yielded, body: ctrPayload("completed", 11, "lint", head), want: 1},
		{name: "empty snapshot triggers on any observed name", snap: empty, entry: yielded, body: ctrPayload("completed", 11, "lint", head), want: 1},
		{name: "a head that is not the anchored head does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", other)},
		{name: "a non-yielded entry does not", snap: required, entry: func(id uuid.UUID) mergequeue.Entry {
			e := ctrYielded(id, head)
			e.YieldedAt, e.YieldReason = nil, ""
			return e
		}, body: ctrPayload("completed", 11, "CI Pass", head)},
		{name: "an entry without intent does not", snap: required, entry: func(id uuid.UUID) mergequeue.Entry {
			e := ctrYielded(id, head)
			e.MergeIntentAt = nil
			return e
		}, body: ctrPayload("completed", 11, "CI Pass", head)},
		{name: "a live entry does not", snap: required, entry: func(id uuid.UUID) mergequeue.Entry {
			e := ctrYielded(id, head)
			e.State = mergequeue.StateLive
			return e
		}, body: ctrPayload("completed", 11, "CI Pass", head)},
		{name: "a non-completed action does not", snap: required, entry: yielded, body: ctrPayload("created", 11, "CI Pass", head)},
		{name: "a delivery without a check-run id does not", snap: required, entry: yielded, body: ctrPayload("completed", 0, "CI Pass", head)},
		// The empty anchor isolates the head_sha guard from the
		// anchored-head comparison, which would otherwise mask it.
		{name: "a delivery without a head does not", snap: required, entry: func(id uuid.UUID) mergequeue.Entry {
			return ctrYielded(id, "")
		}, body: ctrPayload("completed", 11, "CI Pass", "")},
		// A nil snapshot admits every name, so only the name guard
		// refuses an unnamed check run.
		{name: "a delivery without a check name does not", entry: yielded, body: ctrPayload("completed", 11, "", head)},
		{name: "a delivery without a repository does not", snap: required, entry: yielded, body: func() []byte {
			var m map[string]any
			_ = json.Unmarshal(ctrPayload("completed", 11, "CI Pass", head), &m)
			delete(m, "repository")
			b, _ := json.Marshal(m)
			return b
		}(), mutate: func(w *ctrWorld) {
			// A lookup that WOULD answer the malformed URL isolates the
			// repository guard from the run lookup's miss.
			w.srv.cfg.RunRepo = ctrRunRepo{byPR: map[string]*run.Run{"https://github.com//pull/7": w.run}}
		}},
		{name: "an unparseable delivery does not", snap: required, entry: yielded, body: []byte(`{"action":`)},
		{name: "no active entry does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", head),
			mutate: func(w *ctrWorld) { w.queue.found = false }},
		{name: "a queue read error does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", head),
			mutate: func(w *ctrWorld) { w.queue.err = errors.New("queue down") }},
		{name: "a run lookup error does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", head),
			mutate: func(w *ctrWorld) { w.srv.cfg.RunRepo = ctrRunRepo{err: errors.New("db down")} }},
		{name: "no run on the pull request does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", head),
			mutate: func(w *ctrWorld) { w.srv.cfg.RunRepo = ctrRunRepo{} }},
		{name: "an unwired run repository does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", head),
			mutate: func(w *ctrWorld) { w.srv.cfg.RunRepo = nil }},
		{name: "an unwired queue does not", snap: required, entry: yielded, body: ctrPayload("completed", 11, "CI Pass", head),
			mutate: func(w *ctrWorld) { w.srv.cfg.MergeQueue = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newCtrWorld(t, tc.snap, tc.entry)
			if tc.mutate != nil {
				tc.mutate(w)
			}
			probes := &ctrProbes{}
			tr := newCheckRunYieldTrigger(0)
			fired := w.srv.fireCheckRunYieldTrigger(context.Background(), tc.body, tr, probes.probe)
			w.srv.bgMergeQueuePump.Wait()
			if fired != tc.want || probes.count() != tc.want {
				t.Fatalf("fired %d runs and ran %d probes, want %d of each", fired, probes.count(), tc.want)
			}
			if tc.want == 1 && probes.runs[0] != w.run.ID {
				t.Fatalf("probed run %s, want %s", probes.runs[0], w.run.ID)
			}
		})
	}
}

// ctrClock is a settable clock and a captured timer for driving the
// coalescer without wall-clock sleeps.
type ctrClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []ctrTimer
}

type ctrTimer struct {
	d time.Duration
	f func()
}

func (c *ctrClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *ctrClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *ctrClock) after(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, ctrTimer{d: d, f: f})
}

func (c *ctrClock) armed() []ctrTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.timers)
}

// ctrCoalescer is a coalescer on a fake clock with a tracked spawn, and a
// probe that counts its runs and blocks while gate is non-nil.
type ctrCoalescer struct {
	tr    *checkRunYieldTrigger
	clock *ctrClock
	wg    sync.WaitGroup
	mu    sync.Mutex
	n     int
	gate  chan struct{}
}

func newCtrCoalescer(interval time.Duration) *ctrCoalescer {
	clock := &ctrClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	tr := newCheckRunYieldTrigger(interval)
	tr.now, tr.after = clock.Now, clock.after
	return &ctrCoalescer{tr: tr, clock: clock}
}

func (c *ctrCoalescer) spawn(fn func()) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		fn()
	}()
}

func (c *ctrCoalescer) probe() {
	c.mu.Lock()
	c.n++
	gate := c.gate
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (c *ctrCoalescer) probes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *ctrCoalescer) fire(runID uuid.UUID, id int64) checkRunTriggerResult {
	return c.tr.fire(runID, id, c.probe, c.spawn)
}

// waitProbes waits, bounded by timescale.D, until n probes have started.
func (c *ctrCoalescer) waitProbes(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(timescale.D(5 * time.Second))
	for c.probes() < n {
		if time.Now().After(deadline) {
			t.Fatalf("probes started = %d, want %d within the bound", c.probes(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCheckRunTrigger_DedupeByCheckRunID pins the dedupe: a redelivered
// check-run id probes once, a new id probes again.
func TestCheckRunTrigger_DedupeByCheckRunID(t *testing.T) {
	c := newCtrCoalescer(0)
	runID := uuid.New()
	if got := c.fire(runID, 7); got != checkRunTriggerProbed {
		t.Fatalf("first delivery = %s, want probed", got)
	}
	c.wg.Wait()
	if got := c.fire(runID, 7); got != checkRunTriggerDuplicate {
		t.Fatalf("redelivery = %s, want duplicate", got)
	}
	c.wg.Wait()
	if n := c.probes(); n != 1 {
		t.Fatalf("probes = %d after a redelivered check-run id, want 1", n)
	}
	if got := c.fire(runID, 8); got != checkRunTriggerProbed {
		t.Fatalf("a new check-run id = %s, want probed", got)
	}
	c.wg.Wait()
	if n := c.probes(); n != 2 {
		t.Fatalf("probes = %d after a new check-run id, want 2", n)
	}
}

// TestCheckRunTrigger_CoalescesDuringInFlight pins single flight and the one
// follow-up (per process): three triggers while a probe blocks produce
// exactly ONE follow-up probe after it returns, and never a concurrent one.
func TestCheckRunTrigger_CoalescesDuringInFlight(t *testing.T) {
	c := newCtrCoalescer(0)
	c.gate = make(chan struct{})
	runID := uuid.New()
	if got := c.fire(runID, 1); got != checkRunTriggerProbed {
		t.Fatalf("first trigger = %s, want probed", got)
	}
	c.waitProbes(t, 1)
	for id := int64(2); id <= 4; id++ {
		if got := c.fire(runID, id); got != checkRunTriggerCoalesced {
			t.Fatalf("trigger %d during the in-flight probe = %s, want coalesced", id, got)
		}
	}
	if n := c.probes(); n != 1 {
		t.Fatalf("probes = %d while the first is in flight, want 1 (single flight)", n)
	}
	c.mu.Lock()
	gate := c.gate
	c.gate = nil // the follow-up does not block
	c.mu.Unlock()
	close(gate)
	c.wg.Wait()
	if n := c.probes(); n != 2 {
		t.Fatalf("probes = %d after the in-flight probe returned, want 2 (exactly one follow-up)", n)
	}
}

// TestCheckRunTrigger_InsideIntervalSchedulesOneFollowUpNotDropped pins the
// minimum interval without the approval limiter's drop: a trigger inside the
// interval schedules exactly one follow-up at the interval's end, and further
// triggers join it.
func TestCheckRunTrigger_InsideIntervalSchedulesOneFollowUpNotDropped(t *testing.T) {
	interval := timescale.D(time.Minute)
	c := newCtrCoalescer(interval)
	runID := uuid.New()
	c.fire(runID, 1)
	c.wg.Wait()
	c.clock.advance(interval / 4)
	if got := c.fire(runID, 2); got != checkRunTriggerCoalesced {
		t.Fatalf("trigger inside the interval = %s, want coalesced", got)
	}
	if got := c.fire(runID, 3); got != checkRunTriggerCoalesced {
		t.Fatalf("second trigger inside the interval = %s, want coalesced", got)
	}
	c.wg.Wait()
	if n := c.probes(); n != 1 {
		t.Fatalf("probes = %d inside the interval, want 1", n)
	}
	timers := c.clock.armed()
	if len(timers) != 1 {
		t.Fatalf("armed follow-ups = %d, want exactly 1 (never zero, never one per trigger)", len(timers))
	}
	if want := interval - interval/4; timers[0].d != want {
		t.Fatalf("follow-up delay = %v, want %v (the rest of the interval)", timers[0].d, want)
	}
	c.clock.advance(timers[0].d)
	timers[0].f()
	c.wg.Wait()
	if n := c.probes(); n != 2 {
		t.Fatalf("probes = %d after the interval elapsed, want 2 (the follow-up ran)", n)
	}
}

// TestCheckRunTrigger_FollowUpAfterInFlightKeepsTheInterval pins that a
// follow-up recorded during a probe still waits out the minimum interval
// from that probe's start rather than starting the moment it returns.
func TestCheckRunTrigger_FollowUpAfterInFlightKeepsTheInterval(t *testing.T) {
	interval := timescale.D(time.Minute)
	c := newCtrCoalescer(interval)
	c.gate = make(chan struct{})
	runID := uuid.New()
	c.fire(runID, 1)
	c.waitProbes(t, 1)
	c.fire(runID, 2)
	c.mu.Lock()
	gate := c.gate
	c.gate = nil
	c.mu.Unlock()
	close(gate)
	c.wg.Wait()
	if n := c.probes(); n != 1 {
		t.Fatalf("probes = %d when the in-flight probe returned inside the interval, want 1", n)
	}
	timers := c.clock.armed()
	if len(timers) != 1 || timers[0].d != interval {
		t.Fatalf("armed follow-ups = %+v, want one at the full interval %v", timers, interval)
	}
	c.clock.advance(interval)
	timers[0].f()
	c.wg.Wait()
	if n := c.probes(); n != 2 {
		t.Fatalf("probes = %d after the interval, want 2", n)
	}
}

// TestCheckRunTrigger_BoundedState pins the coalescer's memory bounds: the
// per-run seen set evicts its oldest id past checkRunTriggerSeenCap, and an
// idle run's state is pruned after the retention window, while a run with a
// scheduled follow-up is kept.
func TestCheckRunTrigger_BoundedState(t *testing.T) {
	t.Run("seen set evicts the oldest id", func(t *testing.T) {
		c := newCtrCoalescer(0)
		runID := uuid.New()
		for id := int64(1); id <= checkRunTriggerSeenCap+1; id++ {
			c.fire(runID, id)
			c.wg.Wait()
		}
		if got := len(c.tr.runs[runID].seen); got != checkRunTriggerSeenCap {
			t.Fatalf("seen ids = %d, want the cap %d", got, checkRunTriggerSeenCap)
		}
		if got := c.fire(runID, 1); got == checkRunTriggerDuplicate {
			t.Fatal("the evicted oldest id was still deduped; the seen set is unbounded")
		}
		if got := c.fire(runID, checkRunTriggerSeenCap+1); got != checkRunTriggerDuplicate {
			t.Fatalf("the newest id = %s, want duplicate", got)
		}
		c.wg.Wait()
	})
	t.Run("idle state is pruned after the retention", func(t *testing.T) {
		c := newCtrCoalescer(0)
		idle, busy := uuid.New(), uuid.New()
		c.fire(idle, 1)
		c.wg.Wait()
		c.tr.interval = time.Hour * 2 // busy's follow-up waits past the retention
		c.fire(busy, 1)
		c.wg.Wait()
		c.fire(busy, 2) // schedules busy's follow-up
		c.clock.advance(2 * time.Hour)
		c.fire(uuid.New(), 1) // any trigger prunes
		c.wg.Wait()
		if _, ok := c.tr.runs[idle]; ok {
			t.Fatal("an idle run past the retention was kept; the state map is unbounded")
		}
		if r, ok := c.tr.runs[busy]; !ok || !r.waiting {
			t.Fatal("a run with a scheduled follow-up was pruned; its follow-up would be dropped")
		}
		if got := c.fire(idle, 1); got != checkRunTriggerProbed {
			t.Fatalf("the pruned run's redelivery = %s, want probed (fresh state)", got)
		}
		c.wg.Wait()
	})
	t.Run("a wake for a pruned or unscheduled run does nothing", func(t *testing.T) {
		c := newCtrCoalescer(0)
		c.tr.wake(uuid.New())
		c.tr.finish(uuid.New())
		runID := uuid.New()
		c.fire(runID, 1)
		c.wg.Wait()
		c.tr.wake(runID) // not waiting
		c.wg.Wait()
		if n := c.probes(); n != 1 {
			t.Fatalf("probes = %d after a stray wake, want 1", n)
		}
	})
}

// TestWebhook_CheckRunRoutesToYieldTrigger drives the real webhook handler
// with a signed check_run delivery: a required check completing at the
// yielded entry's anchored head runs the re-probe (the queue's second read
// is ReprobeMergeQueueYield's own ActiveForRun), and a non-required check
// reads the queue only for the filter.
func TestWebhook_CheckRunRoutesToYieldTrigger(t *testing.T) {
	const head = "ccc333"
	snap := &run.RequiredChecksSnapshot{Contexts: []string{"CI Pass"}, Sources: []string{"branch_protection"}}
	for _, tc := range []struct {
		name      string
		check     string
		wantReads int
	}{
		{name: "required check probes", check: "CI Pass", wantReads: 2},
		{name: "non-required check does not probe", check: "lint", wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newCtrWorld(t, snap, func(id uuid.UUID) mergequeue.Entry { return ctrYielded(id, head) })
			body := ctrPayload("completed", 42, tc.check, head)
			rec := postWebhook(t, w.srv, map[string]string{
				"X-GitHub-Event":      "check_run",
				"X-GitHub-Delivery":   uuid.NewString(),
				"X-Hub-Signature-256": sign(body),
			}, body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d (%s), want 202", rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			w.srv.bgMergeQueuePump.Wait()
			if got := w.queue.readCount(); got != tc.wantReads {
				t.Fatalf("queue reads = %d, want %d (filter read + the re-probe's own read when it fires)", got, tc.wantReads)
			}
		})
	}
}
