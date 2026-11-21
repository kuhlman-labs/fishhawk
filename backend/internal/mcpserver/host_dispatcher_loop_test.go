package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// host_dispatcher_loop_test.go drives the host-dispatcher tick loop (E88.2 /
// #4526): the REAL core and apiClient against the slice-2 fake backend
// (host_dispatcher_test.go), wrapped here with the list route, a temp state
// dir and a clock seam.

type hdLoopHarness struct {
	*hdHarness
	t          *testing.T
	dir        string
	clock      time.Time
	probe      runnerLivenessVerdict
	srv        *httptest.Server
	mu         sync.Mutex
	created    map[string]time.Time
	listQuery  []string
	terminated []string
	// onDriveView runs on every plain run read (the core's drive view).
	onDriveView func(runID string)
	// listCode non-zero answers the run list with that status.
	listCode int
}

func newHDLoopHarness(t *testing.T) *hdLoopHarness {
	t.Helper()
	h := newHDHarness(t)
	lh := &hdLoopHarness{
		hdHarness: h, t: t, dir: filepath.Join(t.TempDir(), "state"),
		clock: time.Now().UTC(), probe: runnerDead, created: map[string]time.Time{},
	}
	lh.srv = httptest.NewServer(http.HandlerFunc(lh.serve))
	t.Cleanup(lh.srv.Close)
	h.d.r.api.baseURL = lh.srv.URL
	return lh
}

func (lh *hdLoopHarness) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/v0/runs" {
		lh.mu.Lock()
		lh.listQuery = append(lh.listQuery, req.URL.RawQuery)
		code := lh.listCode
		lh.mu.Unlock()
		if code != 0 {
			writeJSON(w, code, apiErrBody("internal", "list failed"))
			return
		}
		state := req.URL.Query().Get("state")
		lh.b.mu.Lock()
		ids := make([]string, 0, len(lh.b.runs))
		for id := range lh.b.runs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		items := []map[string]any{}
		for _, id := range ids {
			r := lh.b.runs[id]
			if (state != "" && r.State != state) || (req.URL.Query().Get("runner_kind") != "" && r.RunnerKind != req.URL.Query().Get("runner_kind")) {
				continue
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(lh.b.runJSON(r, false)), &m)
			lh.mu.Lock()
			m["created_at"] = lh.created[id].Format(time.RFC3339Nano)
			lh.mu.Unlock()
			items = append(items, m)
		}
		lh.b.mu.Unlock()
		out, _ := json.Marshal(map[string]any{"items": items})
		writeJSON(w, 200, string(out))
		return
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if lh.onDriveView != nil && req.Method == http.MethodGet && len(parts) == 3 && req.URL.RawQuery == "" {
		lh.onDriveView(parts[2])
	}
	lh.b.serve(w, req)
}

func (lh *hdLoopHarness) cfg(mut func(*HostDispatcherConfig)) HostDispatcherConfig {
	cfg := HostDispatcherConfig{AllowedRoots: []string{lh.root}, StateDir: lh.dir, MaxInFlight: 10, PerTickCap: 10, DailyCap: 100, Log: io.Discard}
	if mut != nil {
		mut(&cfg)
	}
	out, err := cfg.withDefaults()
	if err != nil {
		lh.t.Fatalf("withDefaults: %v", err)
	}
	return out
}

// loop builds a fresh dispatcher (new core, empty in-process state) over the
// harness's state dir — a second call models a restart.
func (lh *hdLoopHarness) loop(mut func(*HostDispatcherConfig)) *hostDispatchLoop {
	lh.t.Helper()
	cfg := lh.cfg(mut)
	if err := prepareHostDispatchStateDir(cfg.StateDir, fileOwnerUID); err != nil {
		lh.t.Fatalf("prepare state dir: %v", err)
	}
	l := newHostDispatchLoop(cfg, lh.d.r, "/bin/fishhawk-runner")
	l.core.spawn = lh.spawner.spawn
	l.core.probe = func(context.Context, string) runnerLivenessVerdict { return lh.probe }
	l.core.environ = func() []string { return []string{"PATH=/usr/bin"} }
	l.now = func() time.Time { return lh.clock }
	l.lockHeld = func() bool { return true }
	l.terminate = func(runID string) int {
		lh.mu.Lock()
		defer lh.mu.Unlock()
		lh.terminated = append(lh.terminated, runID)
		return 1
	}
	return l
}

func (lh *hdLoopHarness) tick(l *hostDispatchLoop) {
	lh.t.Helper()
	if err := l.tick(context.Background()); err != nil {
		lh.t.Fatalf("tick: %v", err)
	}
}

func (lh *hdLoopHarness) add(createdOffset time.Duration, mut func(*hdRun)) *hdRun {
	r := lh.addRun(mut)
	lh.mu.Lock()
	lh.created[r.ID] = lh.clock.Add(createdOffset)
	lh.mu.Unlock()
	return r
}

func outcomeFor(l *hostDispatchLoop, runID string) (hostDispatchOutcome, bool) {
	for _, o := range l.outcomes {
		if o.RunID == runID {
			return o, true
		}
	}
	return hostDispatchOutcome{}, false
}

func queuedBody() string {
	return `{"error":{"code":"concurrency_slot_queued","message":"queued","details":{"group":"local-plan:devbox","limit":1,"position":1}}}`
}

func markersFor(b *hdBackend, runID string) int {
	return b.count(func(q hdRequest) bool { return isMarkerReq(q) && strings.Contains(q.Path, runID) })
}

// S3.1 / S3.10: a queued stage is admitted only by a LATER POST, and the
// later tick attempts it before an older fresh run.
func TestTick_QueuedStageSpawnedOnLaterTick(t *testing.T) {
	lh := newHDLoopHarness(t)
	queuedRun := lh.add(time.Minute, nil)
	posts := 0
	lh.b.marker = func(runID, _ string) (int, string) {
		if runID == queuedRun.ID {
			posts++
			if posts == 1 {
				return 409, queuedBody()
			}
		}
		return 200, `{"transitioned":true,"stage_state":"dispatched"}`
	}
	l := lh.loop(func(c *HostDispatcherConfig) { c.PerTickCap = 1 })
	lh.tick(l)
	if lh.spawner.n() != 0 || lh.b.stageState(queuedRun.ID, queuedRun.Stages[0].ID) != "awaiting_host_dispatch" {
		t.Fatalf("tick 1: spawns=%d stage=%s; want 0, awaiting_host_dispatch", lh.spawner.n(), lh.b.stageState(queuedRun.ID, queuedRun.Stages[0].ID))
	}
	// An OLDER fresh run arrives; the queued stage must still go first.
	older := lh.add(-time.Hour, nil)
	lh.tick(l)
	if lh.spawner.n() != 1 || lh.spawner.spawns[0].Run != queuedRun.ID {
		t.Fatalf("tick 2 spawns = %+v; want exactly the queued run", lh.spawner.spawns)
	}
	if markersFor(lh.b, older.ID) != 0 {
		t.Fatalf("older fresh run drew a marker before the queued run's episode; per-tick cap 1 should have stopped it")
	}
	if len(lh.b.acts) != 1 {
		t.Fatalf("run_auto_driven rows = %d; want 1 (one per queue episode)", len(lh.b.acts))
	}
}

// S3.10: deterministic order without a queue episode: created_at then id,
// and a run refused for campaign state last tick moves last.
func TestTick_DeterministicOrder(t *testing.T) {
	lh := newHDLoopHarness(t)
	held := lh.add(-3*time.Hour, func(r *hdRun) { r.Link = hdLinked("paused") })
	oldest := lh.add(-2*time.Hour, nil)
	newer := lh.add(-1*time.Hour, nil)
	l := lh.loop(func(c *HostDispatcherConfig) { c.PerTickCap = 1 })
	lh.tick(l) // the held (oldest) run is attempted first and refused for campaign state
	if o, _ := outcomeFor(l, held.ID); o.Refusal != hdRefuseCampaignNotRunning {
		t.Fatalf("tick 1 outcome for held = %+v; want campaign_not_running", o)
	}
	lh.tick(l) // held moves last: oldest goes next
	if lh.spawner.n() != 1 || lh.spawner.spawns[0].Run != oldest.ID {
		t.Fatalf("tick 2 spawns = %+v; want the oldest fresh run", lh.spawner.spawns)
	}
	lh.tick(l)
	if lh.spawner.n() != 2 || lh.spawner.spawns[1].Run != newer.ID {
		t.Fatalf("tick 3 spawns = %+v; want the newer run second", lh.spawner.spawns)
	}
}

// S3.2: a pause file between ticks stops every record-act and marker.
func TestTick_PauseFileBetweenTicks(t *testing.T) {
	lh := newHDLoopHarness(t)
	r := lh.add(0, nil)
	lh.b.marker = func(_, _ string) (int, string) { return 409, queuedBody() }
	l := lh.loop(nil)
	lh.tick(l)
	if markersFor(lh.b, r.ID) != 1 {
		t.Fatalf("tick 1 markers = %d; want 1", markersFor(lh.b, r.ID))
	}
	if err := SetHostDispatcherPaused(lh.dir, true); err != nil {
		t.Fatal(err)
	}
	lh.b.marker = nil
	acts := len(lh.b.acts)
	lh.tick(l)
	if markersFor(lh.b, r.ID) != 1 || len(lh.b.acts) != acts || lh.spawner.n() != 0 {
		t.Fatalf("paused tick: markers=%d acts=%d spawns=%d; want 1, %d, 0", markersFor(lh.b, r.ID), len(lh.b.acts), lh.spawner.n(), acts)
	}
	st, err := ReadHostDispatcherStatus(lh.dir)
	if err != nil || !st.Paused {
		t.Fatalf("status = %+v, %v; want paused", st, err)
	}
	if err := SetHostDispatcherPaused(lh.dir, false); err != nil {
		t.Fatal(err)
	}
	lh.tick(l)
	if lh.spawner.n() != 1 {
		t.Fatalf("resumed tick spawns = %d; want 1", lh.spawner.n())
	}
}

// S3.2b: a pause landing MID-tick (after the tick-start check) is caught by
// the gate before the marker.
func TestTick_PauseFileMidTick(t *testing.T) {
	lh := newHDLoopHarness(t)
	r := lh.add(0, nil)
	lh.onDriveView = func(string) { _ = SetHostDispatcherPaused(lh.dir, true) }
	l := lh.loop(nil)
	lh.tick(l)
	if markersFor(lh.b, r.ID) != 0 || lh.b.count(isRecordReq) != 0 || lh.spawner.n() != 0 {
		t.Fatalf("mid-tick pause: markers=%d acts=%d spawns=%d; want 0, 0, 0", markersFor(lh.b, r.ID), lh.b.count(isRecordReq), lh.spawner.n())
	}
	if o, _ := outcomeFor(l, r.ID); o.Refusal != hdRefusePaused {
		t.Fatalf("outcome = %+v; want paused", o)
	}
}

// S3.3: a campaign paused while a stage is queued: the next tick's fresh link
// read refuses although the marker would admit.
func TestTick_CampaignPausedWhileQueued(t *testing.T) {
	lh := newHDLoopHarness(t)
	r := lh.add(0, func(r *hdRun) { r.Link = hdLinked("running") })
	lh.b.marker = func(_, _ string) (int, string) { return 409, queuedBody() }
	l := lh.loop(nil)
	lh.tick(l)
	lh.b.mu.Lock()
	r.Link.Campaigns[0].State = "paused"
	lh.b.mu.Unlock()
	lh.b.marker = nil
	lh.tick(l)
	if markersFor(lh.b, r.ID) != 1 || lh.spawner.n() != 0 {
		t.Fatalf("paused campaign: markers=%d spawns=%d; want 1 (tick 1 only), 0", markersFor(lh.b, r.ID), lh.spawner.n())
	}
	if o, _ := outcomeFor(l, r.ID); o.Refusal != hdRefuseCampaignNotRunning {
		t.Fatalf("tick 2 outcome = %+v; want campaign_not_running", o)
	}
}

func seedLedger(t *testing.T, dir string, l hdLedger) {
	t.Helper()
	if err := prepareHostDispatchStateDir(dir, fileOwnerUID); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(l)
	if err := writeHostDispatchFile(dir, hostDispatchLedgerFile, raw); err != nil {
		t.Fatal(err)
	}
}

// S3.4: two ledger entries the server reports running fill max in-flight 2:
// a third eligible run gets no marker.
func TestTick_MaxInFlightNoMarker(t *testing.T) {
	lh := newHDLoopHarness(t)
	busy := func(r *hdRun) {
		r.AutoDispatch = false
		r.Stages[0].State = "running"
	}
	a, b := lh.add(0, busy), lh.add(0, busy)
	third := lh.add(0, nil)
	seedLedger(t, lh.dir, hdLedger{InFlight: map[string]hdLedgerEntry{
		a.Stages[0].ID: {RunID: a.ID, StageID: a.Stages[0].ID},
		b.Stages[0].ID: {RunID: b.ID, StageID: b.Stages[0].ID},
	}})
	l := lh.loop(func(c *HostDispatcherConfig) { c.MaxInFlight = 2 })
	lh.tick(l)
	if markersFor(lh.b, third.ID) != 0 || lh.spawner.n() != 0 {
		t.Fatalf("over max in-flight: markers=%d spawns=%d; want 0, 0", markersFor(lh.b, third.ID), lh.spawner.n())
	}
	if o, _ := outcomeFor(l, third.ID); o.Refusal != hdRefuseCapInFlight {
		t.Fatalf("outcome = %+v; want %s", o, hdRefuseCapInFlight)
	}
	// One entry ends: the third run is admitted.
	lh.b.setState(lh.b.runs[a.ID], a.Stages[0].ID, "succeeded")
	lh.tick(l)
	if lh.spawner.n() != 1 {
		t.Fatalf("after a slot freed: spawns = %d; want 1", lh.spawner.n())
	}
}

// S3.5: the per-tick cap bounds campaign-link reads and marker POSTs.
func TestTick_PerTickCapBoundsReadsAndPosts(t *testing.T) {
	lh := newHDLoopHarness(t)
	for i := 0; i < 6; i++ {
		lh.add(time.Duration(i)*time.Minute, nil)
	}
	l := lh.loop(func(c *HostDispatcherConfig) { c.PerTickCap = 4; c.MaxInFlight = 10 })
	lh.tick(l)
	if n := lh.b.count(isLinkReq); n != 4 {
		t.Fatalf("campaign-link reads = %d; want 4", n)
	}
	if n := lh.b.count(isMarkerReq); n != 4 {
		t.Fatalf("marker POSTs = %d; want 4", n)
	}
	st, _ := ReadHostDispatcherStatus(lh.dir)
	if st.LastRefusalCode != hdRefuseCapPerTick {
		t.Fatalf("last refusal = %q; want %s", st.LastRefusalCode, hdRefuseCapPerTick)
	}
	// #5402 R5: the per-tick cap is global-only, never a per-run entry.
	for id, ref := range st.RunRefusals {
		if ref.Code == hdRefuseCapPerTick {
			t.Fatalf("run_refusals[%s] = %+v; %s must stay global-only", id, ref, hdRefuseCapPerTick)
		}
	}
}

// S3.6: the daily cap is persisted and rolls over at 00:00:00Z.
func TestTick_DailyCapRollsOverAtUTCMidnight(t *testing.T) {
	lh := newHDLoopHarness(t)
	lh.clock = time.Date(2026, 11, 12, 23, 58, 0, 0, time.UTC)
	lh.probe = runnerLive // already-spawned stages are never re-spawned here
	var runs []*hdRun
	for i := 0; i < 4; i++ {
		runs = append(runs, lh.add(time.Duration(i)*time.Second, nil))
	}
	capCfg := func(c *HostDispatcherConfig) { c.DailyCap = 3 }
	l := lh.loop(capCfg)
	lh.tick(l)
	if lh.spawner.n() != 3 {
		t.Fatalf("spawns = %d; want 3", lh.spawner.n())
	}
	if o, _ := outcomeFor(l, runs[3].ID); o.Refusal != hdRefuseCapDaily {
		t.Fatalf("fourth = %+v; want %s", o, hdRefuseCapDaily)
	}
	lh.clock = time.Date(2026, 11, 12, 23, 59, 0, 0, time.UTC)
	restarted := lh.loop(capCfg)
	lh.tick(restarted)
	if lh.spawner.n() != 3 {
		t.Fatalf("restarted dispatcher spawned past the persisted daily count: %d", lh.spawner.n())
	}
	if o, _ := outcomeFor(restarted, runs[3].ID); o.Refusal != hdRefuseCapDaily {
		t.Fatalf("restarted fourth = %+v; want %s", o, hdRefuseCapDaily)
	}
	lh.clock = time.Date(2026, 11, 13, 0, 0, 5, 0, time.UTC)
	lh.tick(restarted)
	if lh.spawner.n() != 4 || lh.spawner.spawns[3].Run != runs[3].ID {
		t.Fatalf("next UTC day spawns = %d; want the fourth run spawned", lh.spawner.n())
	}
}

// ADR-104 A1: with no daily cap configured (DailyCap 0) the dispatcher spawns
// past the old default of 48. The ledger is seeded at exactly 48 for the
// current UTC day, so a restored default would refuse the first candidate;
// spawns_today stays an observation and the status file names no daily limit.
func TestTick_NoDailyCapSpawnsPast48(t *testing.T) {
	lh := newHDLoopHarness(t)
	first := lh.add(0, nil)
	second := lh.add(time.Second, nil)
	l := lh.loop(func(c *HostDispatcherConfig) { c.DailyCap = 0 })
	seed, err := json.Marshal(hdLedger{
		Day:         lh.clock.UTC().Format(time.DateOnly),
		SpawnsToday: 48,
		InFlight:    map[string]hdLedgerEntry{},
		Respawns:    map[string]bool{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lh.dir, hostDispatchLedgerFile), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	lh.tick(l)
	if lh.spawner.n() != 2 {
		t.Fatalf("spawns = %d; want both runs spawned with no daily cap", lh.spawner.n())
	}
	for _, r := range []*hdRun{first, second} {
		o, ok := outcomeFor(l, r.ID)
		if !ok || o.Refusal == hdRefuseCapDaily {
			t.Fatalf("run %s outcome = %+v (found %v); want no %s", r.ID, o, ok, hdRefuseCapDaily)
		}
	}
	st, err := ReadHostDispatcherStatus(lh.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.SpawnsToday != 50 || st.DailyCap != 0 {
		t.Fatalf("status spawns_today=%d daily_cap=%d; want 50 and 0", st.SpawnsToday, st.DailyCap)
	}
	raw, err := os.ReadFile(filepath.Join(lh.dir, hostDispatchStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "daily_cap") {
		t.Fatalf("status.json names a daily limit with none set: %s", raw)
	}
}

// S3.7: the stale re-spawn counter survives a restart; a new marker epoch
// allows one more.
func TestTick_StaleRespawnPersistedAcrossRestart(t *testing.T) {
	lh := newHDLoopHarness(t)
	old := lh.clock.Add(-2 * time.Hour)
	r := lh.add(0, func(r *hdRun) { r.Stages[0].State = "dispatched"; r.Stages[0].UpdatedAt = old })
	a := lh.loop(nil)
	lh.tick(a)
	if lh.spawner.n() != 1 {
		t.Fatalf("dispatcher A spawns = %d; want one stale re-spawn", lh.spawner.n())
	}
	if o, _ := outcomeFor(a, r.ID); !o.Respawn {
		t.Fatalf("A outcome = %+v; want a respawn", o)
	}
	b := lh.loop(nil)
	lh.tick(b)
	if lh.spawner.n() != 1 {
		t.Fatalf("restarted dispatcher B re-spawned the same epoch: spawns = %d", lh.spawner.n())
	}
	if o, _ := outcomeFor(b, r.ID); o.Refusal != hdRefuseStaleRespawnCap {
		t.Fatalf("B outcome = %+v; want %s", o, hdRefuseStaleRespawnCap)
	}
	lh.b.mu.Lock()
	r.Stages[0].UpdatedAt = old.Add(time.Minute) // a new marker epoch
	lh.b.mu.Unlock()
	c := lh.loop(nil)
	lh.tick(c)
	if lh.spawner.n() != 2 {
		t.Fatalf("new epoch spawns = %d; want one more", lh.spawner.n())
	}
}

// S3.8: a run this process spawned turns cancelled; its runners are reaped.
func TestTick_ReapsOwnRunnerOnCancel(t *testing.T) {
	lh := newHDLoopHarness(t)
	r := lh.add(0, nil)
	other := lh.add(0, func(r *hdRun) { r.AutoDispatch = false })
	l := lh.loop(nil)
	lh.tick(l)
	if lh.spawner.n() != 1 {
		t.Fatalf("spawns = %d; want 1", lh.spawner.n())
	}
	lh.b.mu.Lock()
	r.State = "cancelled"
	other.State = "cancelled"
	lh.b.mu.Unlock()
	lh.tick(l)
	lh.tick(l)
	if len(lh.terminated) != 1 || lh.terminated[0] != r.ID {
		t.Fatalf("terminated = %v; want exactly this process's run %s once", lh.terminated, r.ID)
	}
	if n := len(l.ledger.InFlight); n != 0 {
		t.Fatalf("ledger in-flight = %d after the reap; want 0", n)
	}
}

// F7(b): a run whose linked campaign is cancelled is reaped although the run
// itself is still running.
func TestTick_ReapsOwnRunnerOnCampaignCancel(t *testing.T) {
	lh := newHDLoopHarness(t)
	r := lh.add(0, func(r *hdRun) { r.Link = hdLinked("running") })
	l := lh.loop(nil)
	lh.tick(l)
	if lh.spawner.n() != 1 {
		t.Fatalf("spawns = %d; want 1", lh.spawner.n())
	}
	lh.tick(l)
	if len(lh.terminated) != 0 {
		t.Fatalf("running campaign reaped: %v", lh.terminated)
	}
	lh.b.mu.Lock()
	r.Link.Campaigns[0].State = "cancelled"
	lh.b.mu.Unlock()
	lh.tick(l)
	if len(lh.terminated) != 1 || lh.terminated[0] != r.ID {
		t.Fatalf("terminated = %v; want %s after its campaign was cancelled", lh.terminated, r.ID)
	}
}

// S3.9: only pending and running local runs are listed, so a cancelled run's
// parked stage is never a candidate.
func TestTick_CancelledRunNotSpawned(t *testing.T) {
	lh := newHDLoopHarness(t)
	r := lh.add(0, func(r *hdRun) { r.State = "cancelled" })
	l := lh.loop(nil)
	lh.tick(l)
	if markersFor(lh.b, r.ID) != 0 || lh.spawner.n() != 0 {
		t.Fatalf("cancelled run: markers=%d spawns=%d; want 0, 0", markersFor(lh.b, r.ID), lh.spawner.n())
	}
	for _, q := range lh.listQuery {
		v := mustQuery(t, q)
		if v.Get("runner_kind") != "local" || (v.Get("state") != "pending" && v.Get("state") != "running") {
			t.Fatalf("list query %q; want runner_kind=local and state pending|running", q)
		}
	}
	if len(lh.listQuery) != 2 {
		t.Fatalf("list queries = %v; want one per state", lh.listQuery)
	}
}

func mustQuery(t *testing.T, raw string) interface{ Get(string) string } {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://x/?"+raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req.URL.Query()
}

// S3.11: the status file carries the tick fields, mode 0600.
func TestStatusFile_ReportsTickFields(t *testing.T) {
	lh := newHDLoopHarness(t)
	lh.add(0, nil)
	blocked := lh.add(time.Minute, nil)
	l := lh.loop(func(c *HostDispatcherConfig) { c.DailyCap = 1 })
	lh.tick(l)
	st, err := ReadHostDispatcherStatus(lh.dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Paused || !st.LastTickAt.Equal(lh.clock) || st.SpawnsToday != 1 || st.SpawnsDay != lh.clock.Format(time.DateOnly) || st.InFlight != 1 {
		t.Fatalf("status = %+v; want unpaused, last_tick_at=clock, 1 spawn today, 1 in flight", st)
	}
	if st.LastRefusalCode != hdRefuseCapDaily || st.LastRefusalRunID != blocked.ID || st.PID != os.Getpid() {
		t.Fatalf("status refusal = %q run=%s pid=%d; want %s for %s", st.LastRefusalCode, st.LastRefusalRunID, st.PID, hdRefuseCapDaily, blocked.ID)
	}
	if st.DailyCap != 1 {
		t.Fatalf("status daily_cap = %d; want the opt-in cap 1", st.DailyCap)
	}
	raw, err := os.ReadFile(filepath.Join(lh.dir, hostDispatchStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"daily_cap":1`) {
		t.Fatalf("status.json lacks the daily_cap key: %s", raw)
	}
	if st.Stale(lh.clock.Add(89*time.Second)) || !st.Stale(lh.clock.Add(91*time.Second)) {
		t.Fatal("staleness is not 3 x the 30s interval")
	}
	assertMode(t, filepath.Join(lh.dir, hostDispatchStatusFile), 0o600)
	assertMode(t, filepath.Join(lh.dir, hostDispatchLedgerFile), 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Fatalf("%s mode = %o; want %o", path, fi.Mode().Perm(), want)
	}
}

// A ledger write failing at the reservation refuses with no record or marker.
func TestTick_LedgerWriteFailureRefuses(t *testing.T) {
	lh := newHDLoopHarness(t)
	r := lh.add(0, nil)
	l := lh.loop(nil)
	writes := 0
	l.writeFile = func(dir, name string, data []byte) error {
		if name == hostDispatchLedgerFile {
			writes++
			if writes == 2 { // the reservation (the first is the tick's own prune)
				return errors.New("disk full")
			}
		}
		return writeHostDispatchFile(dir, name, data)
	}
	lh.tick(l)
	if markersFor(lh.b, r.ID) != 0 || lh.b.count(isRecordReq) != 0 || lh.spawner.n() != 0 {
		t.Fatalf("ledger write failure: markers=%d acts=%d spawns=%d; want 0, 0, 0", markersFor(lh.b, r.ID), lh.b.count(isRecordReq), lh.spawner.n())
	}
	if o, _ := outcomeFor(l, r.ID); o.Refusal != hdRefuseLedgerWrite {
		t.Fatalf("outcome = %+v; want %s", o, hdRefuseLedgerWrite)
	}
	if l.ledger.SpawnsToday != 0 || len(l.ledger.InFlight) != 0 {
		t.Fatalf("refused reservation left state: %+v", l.ledger)
	}
}

// d27d6170 (persisted counter): a stale re-spawn whose record act fails is
// returned to the ledger, so the next tick still re-spawns once and a restart
// then refuses; a Release whose ledger write fails keeps the key (the epoch
// stays consumed, fail safe). Counterfactual run by hand: deleting the
// re-add after the failed persist let Allowed answer true, RED.
func TestTick_StaleRespawnReleasePersisted(t *testing.T) {
	lh := newHDLoopHarness(t)
	old := lh.clock.Add(-2 * time.Hour)
	r := lh.add(0, func(r *hdRun) { r.Stages[0].State = "dispatched"; r.Stages[0].UpdatedAt = old })
	lh.b.recordCode = 500
	a := lh.loop(nil)
	lh.tick(a)
	if o, _ := outcomeFor(a, r.ID); o.Refusal != hdRefuseRecordFailed || lh.spawner.n() != 0 {
		t.Fatalf("tick 1 = %+v spawns=%d; want record_act_failed, no spawn", o, lh.spawner.n())
	}
	if len(a.ledger.Respawns) != 0 {
		t.Fatalf("ledger respawns after a pre-spawn failure = %v; want the reservation returned", a.ledger.Respawns)
	}
	lh.b.recordCode = 0
	lh.tick(a)
	if lh.spawner.n() != 1 {
		t.Fatalf("tick 2 spawns = %d; want the epoch's one re-spawn", lh.spawner.n())
	}
	b := lh.loop(nil)
	lh.tick(b)
	if o, _ := outcomeFor(b, r.ID); o.Refusal != hdRefuseStaleRespawnCap {
		t.Fatalf("restart = %+v; want %s", o, hdRefuseStaleRespawnCap)
	}

	epoch := lh.clock.Add(-time.Hour)
	if ok, err := b.Reserve("stage-x", epoch); !ok || err != nil {
		t.Fatalf("Reserve = %v, %v", ok, err)
	}
	b.writeFile = func(string, string, []byte) error { return errors.New("disk full") }
	if err := b.Release("stage-x", epoch); err == nil {
		t.Fatal("Release with a failing ledger write returned nil")
	}
	if b.Allowed("stage-x", epoch) {
		t.Fatal("a failed Release freed the epoch; it must stay consumed")
	}
}

// A slot-queued answer releases the reservation (no spawn counted).
func TestTick_QueuedReleasesReservation(t *testing.T) {
	lh := newHDLoopHarness(t)
	lh.add(0, nil)
	lh.b.marker = func(_, _ string) (int, string) { return 409, queuedBody() }
	l := lh.loop(nil)
	lh.tick(l)
	if l.ledger.SpawnsToday != 0 || len(l.ledger.InFlight) != 0 {
		t.Fatalf("queued attempt kept the reservation: %+v", l.ledger)
	}
}

// S3.12 / R8: state-dir refusals and modes.
func TestStateDir_Refusals(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareHostDispatchStateDir(link, fileOwnerUID); err == nil || !strings.Contains(err.Error(), hdStateDirSymlink) {
		t.Fatalf("symlink state dir: err = %v; want %s", err, hdStateDirSymlink)
	}
	otherUID := func(os.FileInfo) (int, bool) { return os.Getuid() + 1, true }
	if err := prepareHostDispatchStateDir(real, otherUID); err == nil || !strings.Contains(err.Error(), hdStateDirNotOwned) {
		t.Fatalf("foreign-owned state dir: err = %v; want %s", err, hdStateDirNotOwned)
	}
	noUID := func(os.FileInfo) (int, bool) { return 0, false }
	if err := prepareHostDispatchStateDir(real, noUID); err == nil || !strings.Contains(err.Error(), hdStateDirNotOwned) {
		t.Fatalf("unverifiable owner: err = %v; want %s", err, hdStateDirNotOwned)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareHostDispatchStateDir(file, fileOwnerUID); err == nil || !strings.Contains(err.Error(), hdStateDirNotDir) {
		t.Fatalf("file state dir: err = %v; want %s", err, hdStateDirNotDir)
	}
	created := filepath.Join(base, "a", "b")
	if err := prepareHostDispatchStateDir(created, fileOwnerUID); err != nil {
		t.Fatal(err)
	}
	assertMode(t, created, 0o700)
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareHostDispatchStateDir(loose, fileOwnerUID); err != nil {
		t.Fatal(err)
	}
	assertMode(t, loose, 0o700)
	if err := SetHostDispatcherPaused(loose, true); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(loose, hostDispatchPauseFile), 0o600)
	if err := writeHostDispatchFile(loose, hostDispatchLedgerFile, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(loose, hostDispatchLedgerFile), 0o600)
	if uid, ok := fileOwnerUID(mustStat(t, loose)); !ok || uid != os.Getuid() {
		t.Fatalf("fileOwnerUID = %d, %v; want the current uid", uid, ok)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// F7(e): the state-dir flock refuses a second dispatcher (naming the holder
// pid), is released when the holder's file closes, and leaves the lock file in
// place. Counterfactual run by hand: replacing the Flock call with a nil error
// let the second acquire succeed, RED.
func TestStateDirLock_SecondDispatcherRefused(t *testing.T) {
	dir := t.TempDir()
	a, err := acquireHostDispatchLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = acquireHostDispatchLock(dir)
	if err == nil || !strings.Contains(err.Error(), hdStateDirLocked) || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("second lock: err = %v; want %s naming pid %d", err, hdStateDirLocked, os.Getpid())
	}
	assertMode(t, filepath.Join(dir, hostDispatchLockFile), 0o600)
	a.unlock()
	if _, err := os.Stat(filepath.Join(dir, hostDispatchLockFile)); err != nil {
		t.Fatalf("unlock removed the lock file (a contender could lock a fresh inode): %v", err)
	}
	b, err := acquireHostDispatchLock(dir)
	if err != nil {
		t.Fatalf("lock not free after the holder released it: %v", err)
	}
	b.unlock()
}

// An unopenable lock path (the state dir is missing) refuses state_dir_locked
// naming the path, and a lock file that was removed and re-created between the
// open and the post-flock identity check is refused and closed, never held.
// Counterfactual run by hand: making held() return true let the replaced file
// through with no error, RED.
func TestStateDirLock_OpenAndReplacedRefusals(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := acquireHostDispatchLock(missing); err == nil || !strings.Contains(err.Error(), hdStateDirLocked) || !strings.Contains(err.Error(), filepath.Join(missing, hostDispatchLockFile)) {
		t.Fatalf("missing state dir: err = %v; want %s naming the lock path", err, hdStateDirLocked)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, hostDispatchLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := finishHostDispatchLock(f, path)
	if err == nil || l != nil || !strings.Contains(err.Error(), hdStateDirLocked) || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("replaced lock file: lock = %v, err = %v; want %s ... was replaced", l, err, hdStateDirLocked)
	}
	if _, serr := f.Stat(); serr == nil {
		t.Fatal("the refused lock's file is still open")
	}
}

// hdContend starts n contenders at once over dir. Each winner holds the lock
// for a while and records how many winners were inside together.
func hdContend(t *testing.T, dir string, n int) (winners, maxInside int) {
	t.Helper()
	var (
		mu     sync.Mutex
		inside int
		wg     sync.WaitGroup
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lock, err := acquireHostDispatchLock(dir)
			if err != nil {
				if !strings.Contains(err.Error(), hdStateDirLocked) {
					t.Errorf("contender error = %v; want %s", err, hdStateDirLocked)
				}
				return
			}
			mu.Lock()
			winners++
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			time.Sleep(50 * time.Millisecond) // the protected tick work
			if !lock.held() {
				t.Error("a winner lost its lock mid-hold")
			}
			mu.Lock()
			inside--
			mu.Unlock()
			lock.unlock()
		}()
	}
	close(start)
	wg.Wait()
	return winners, maxInside
}

// 8c4a6c05: concurrent dispatcher starts over one state dir; at most one is
// ever inside the protected work. Counterfactual run by hand: replacing the
// Flock call with a nil error let every contender in together, RED.
func TestStateDirLock_ConcurrentStartsExclusive(t *testing.T) {
	for round := 0; round < 5; round++ {
		winners, maxInside := hdContend(t, t.TempDir(), 16)
		if winners < 1 || maxInside != 1 {
			t.Fatalf("round %d: winners=%d max inside together=%d; want >=1 and exactly 1", round, winners, maxInside)
		}
	}
}

// 8c4a6c05: a dead holder's lock (an old pid file, a crashed process whose
// file the kernel closed) is taken over by exactly one of many concurrent
// contenders; none of them removes another's lock. Counterfactual run by
// hand: replacing the Flock call with a nil error let every contender take
// it over together, RED.
func TestStateDirLock_StaleTakeoverExclusive(t *testing.T) {
	dir := t.TempDir()
	// A pre-flock pid file naming a dead pid, then a holder that crashes: its
	// file is closed without the unlock path (the kernel's release on exit).
	if err := os.WriteFile(filepath.Join(dir, hostDispatchLockFile), []byte("2147483646"), 0o600); err != nil {
		t.Fatal(err)
	}
	crashed, err := acquireHostDispatchLock(dir)
	if err != nil {
		t.Fatalf("a stale pid file blocked the lock: %v", err)
	}
	_ = crashed.f.Close()
	for round := 0; round < 5; round++ {
		winners, maxInside := hdContend(t, dir, 16)
		if winners < 1 || maxInside != 1 {
			t.Fatalf("round %d: winners=%d max inside together=%d; want >=1 and exactly 1", round, winners, maxInside)
		}
	}
}

// A dispatcher whose lock file was removed and re-created (so another
// dispatcher could lock the new file) stops at its next tick before any
// request. Counterfactual run by hand: making held() return true let the
// tick proceed with no lock-lost error, RED.
func TestStateDirLock_ReplacedLockStopsTick(t *testing.T) {
	lh := newHDLoopHarness(t)
	l := lh.loop(nil)
	lock, err := acquireHostDispatchLock(lh.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.unlock()
	l.lockHeld = lock.held
	lockPath := filepath.Join(lh.dir, hostDispatchLockFile)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	other, err := acquireHostDispatchLock(lh.dir)
	if err != nil {
		t.Fatalf("the replacement lock: %v", err)
	}
	defer other.unlock()
	lh.add(0, nil)
	if err := l.tick(context.Background()); err == nil || !strings.Contains(err.Error(), hdRefuseLockLost) {
		t.Fatalf("tick with a replaced lock: err = %v; want %s", err, hdRefuseLockLost)
	}
	if lh.b.count(func(hdRequest) bool { return true }) != 0 || len(lh.listQuery) != 0 {
		t.Fatal("a dispatcher that lost its lock still made requests")
	}
}

// 667525a3: older runs whose marker the server keeps refusing (later-wave
// children held at 409 dependency_not_satisfied) cannot spend every tick's
// budget: a held run moves last, so a newer admissible run is spawned on the
// next tick, and each held dispatch records one row, not one per tick.
// Counterfactual run by hand: dropping `|| o.Held` from record() left the
// newer run unspawned after 3 ticks, RED.
func TestTick_HeldOlderRunsDoNotStarveNewer(t *testing.T) {
	lh := newHDLoopHarness(t)
	held := map[string]bool{}
	for i := 0; i < hostDispatchDefaultPerTick; i++ {
		r := lh.add(-time.Duration(10-i)*time.Hour, nil)
		held[r.ID] = true
	}
	newer := lh.add(0, nil)
	lh.b.marker = func(runID, _ string) (int, string) {
		if held[runID] {
			return 409, apiErrBody("dependency_not_satisfied", "wave 1 is not integrated")
		}
		return 200, `{"transitioned":true,"stage_state":"dispatched"}`
	}
	l := lh.loop(func(c *HostDispatcherConfig) { c.PerTickCap = hostDispatchDefaultPerTick })
	lh.tick(l)
	if markersFor(lh.b, newer.ID) != 0 {
		t.Fatal("tick 1 reached the newer run; the fixture needs per-tick-cap held runs ahead of it")
	}
	for tick := 2; tick <= 3 && lh.spawner.n() == 0; tick++ {
		lh.tick(l)
	}
	if lh.spawner.n() != 1 || lh.spawner.spawns[0].Run != newer.ID {
		t.Fatalf("spawns = %+v; want the newer run spawned by tick 3", lh.spawner.spawns)
	}
	if len(lh.b.acts) != hostDispatchDefaultPerTick+1 {
		t.Fatalf("run_auto_driven rows = %d; want one per held dispatch plus the spawn (%d)", len(lh.b.acts), hostDispatchDefaultPerTick+1)
	}
}

// #5246: within the deprioritised rank, a run held once and admissible again
// is reached even while older runs stay held. PerTickCap+1 older runs are held
// forever and one newer run is held until the test clears it. Rank 2 rotates
// least-recently-evaluated first, so after the hold clears the newer run is
// re-evaluated within ceil((held+1)/PerTickCap) = 2 ticks (the bound holds over
// ticks that COMPLETE the candidate pass without a host_memory_pressure stop
// and while ranks 0 and 1 leave budget; this fixture has neither). Under a
// created_at-only rank 2 the first PerTickCap older runs take the whole budget
// on every tick and the newer run is never evaluated.
// Counterfactuals run by hand, each RED: `if la != lb {` -> `if false {` in
// order (the newer run is starved); deleting the `l.lastEvaluated[run.ID] =
// l.evalTick` stamp (every stamp reads 0, same starvation); deleting the
// `delete(l.lastEvaluated, id)` prune (the post-spawn bounded-map assertion).
func TestTick_OnceHeldNewerRunReachedWithinDeprioritisedRank(t *testing.T) {
	lh := newHDLoopHarness(t)
	perTick := hostDispatchDefaultPerTick
	older := map[string]bool{}
	for i := 0; i < perTick+1; i++ {
		r := lh.add(-time.Duration(10-i)*time.Hour, nil)
		older[r.ID] = true
	}
	newer := lh.add(0, nil)
	var newerHeld atomic.Bool
	newerHeld.Store(true)
	lh.b.marker = func(runID, _ string) (int, string) {
		if older[runID] || (runID == newer.ID && newerHeld.Load()) {
			return 409, apiErrBody("dependency_not_satisfied", "wave 1 is not integrated")
		}
		return 200, `{"transitioned":true,"stage_state":"dispatched"}`
	}
	l := lh.loop(func(c *HostDispatcherConfig) { c.PerTickCap = perTick })

	postsBefore := 0
	tickBounded := func(label string) {
		t.Helper()
		lh.tick(l)
		total := lh.b.count(isMarkerReq)
		if got := total - postsBefore; got > perTick {
			t.Fatalf("%s: %d marker POSTs; the per-tick budget bounds them at %d", label, got, perTick)
		}
		postsBefore = total
	}

	// Fixture guard: the newer run must be held (and so deprioritised) by tick 2.
	heldAt := 0
	for tick := 1; tick <= 2 && heldAt == 0; tick++ {
		tickBounded("hold phase")
		if o, ok := outcomeFor(l, newer.ID); ok && o.Held && o.Refusal == "dependency_not_satisfied" {
			heldAt = tick
		}
	}
	if heldAt == 0 {
		t.Fatalf("the newer run was not held by tick 2 (outcome %+v); the fixture needs it evaluated and held", l.outcomes)
	}
	if lh.spawner.n() != 0 {
		t.Fatalf("spawns = %+v during the hold; want none", lh.spawner.spawns)
	}

	newerHeld.Store(false)
	deprioritised := perTick + 2                     // perTick+1 older runs plus the newer one
	bound := (deprioritised + perTick - 1) / perTick // ceil(N / PerTickCap)
	for tick := 1; tick <= bound && lh.spawner.n() == 0; tick++ {
		tickBounded("after the hold cleared")
	}
	if lh.spawner.n() != 1 || lh.spawner.spawns[0].Run != newer.ID {
		t.Fatalf("spawns = %+v; want exactly the newer run within %d ticks of the hold clearing", lh.spawner.spawns, bound)
	}

	// Pruning: the spawned run left the deprioritised set, and the rotation
	// state is bounded by that set.
	if _, ok := l.lastEvaluated[newer.ID]; ok {
		t.Fatalf("lastEvaluated still has the spawned run %s; the map must be pruned to the deprioritised set", newer.ID)
	}
	for id := range l.lastEvaluated {
		if !l.deprioritised[id] {
			t.Fatalf("lastEvaluated[%s] outlives the deprioritised set %v", id, l.deprioritised)
		}
	}
}

// #5246: order's rank 2 is least-recently-evaluated first, then created_at and
// id. The seeded lastEvaluated values deliberately invert created_at order, so
// a created_at-only rank 2 fails the assertion.
func TestOrder_DeprioritisedLeastRecentlyEvaluatedFirst(t *testing.T) {
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	run := func(id string, createdOffset time.Duration) Run {
		return Run{ID: id, CreatedAt: base.Add(createdOffset)}
	}
	l := &hostDispatchLoop{
		queued: map[string]time.Time{
			"q-late":  base.Add(2 * time.Minute),
			"q-early": base.Add(time.Minute),
		},
		deprioritised: map[string]bool{"d-old": true, "d-mid": true, "d-new": true, "d-tie-x": true, "d-tie-y": true},
		lastEvaluated: map[string]uint64{"d-old": 5, "d-mid": 2, "d-new": 2, "d-tie-x": 7, "d-tie-y": 7},
	}
	runs := []Run{
		run("d-tie-y", time.Hour),
		run("d-new", 3*time.Hour),
		run("n-new", 5*time.Hour),
		run("d-old", 0),
		run("q-late", 9*time.Hour),
		run("d-tie-x", time.Hour),
		run("n-old", 4*time.Hour),
		run("d-mid", 2*time.Hour),
		run("q-early", 8*time.Hour),
	}
	l.order(runs)
	var got []string
	for _, r := range runs {
		got = append(got, r.ID)
	}
	want := []string{
		"q-early", "q-late", // rank 0: oldest queue episode first
		"n-old", "n-new", // rank 1: created_at
		"d-mid", "d-new", // rank 2: lastEvaluated 2, then created_at
		"d-old",              // lastEvaluated 5, though it is the oldest run
		"d-tie-x", "d-tie-y", // lastEvaluated 7, same created_at: id
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v; want %v", got, want)
	}
}

// RunHostDispatcher refuses an unconfined or out-of-bound configuration
// before touching the state dir.
func TestRunHostDispatcher_ConfigRefusals(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	ctx := context.Background()
	if err := RunHostDispatcher(ctx, HostDispatcherConfig{StateDir: dir}); err == nil || !strings.Contains(err.Error(), "allowed root") {
		t.Fatalf("no roots: err = %v", err)
	}
	if err := RunHostDispatcher(ctx, HostDispatcherConfig{StateDir: dir, AllowedRoots: []string{"/tmp"}, Interval: 46 * time.Second}); err == nil || !strings.Contains(err.Error(), "45s") {
		t.Fatalf("46s interval: err = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused config created the state dir: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	err := RunHostDispatcher(cctx, HostDispatcherConfig{StateDir: dir, AllowedRoots: []string{"/tmp"}, BackendURL: "http://127.0.0.1:1", RunnerBinary: "/bin/true", Log: io.Discard})
	if err != nil {
		t.Fatalf("pre-cancelled run: %v; want one tick then a clean exit", err)
	}
	st, serr := ReadHostDispatcherStatus(dir)
	if serr != nil || st.LastTickAt.IsZero() || st.LastRefusalCode != hdRefuseListFailed {
		t.Fatalf("status after one tick = %+v, %v; want a tick with list_runs_failed", st, serr)
	}
	lock, lerr := acquireHostDispatchLock(dir)
	if lerr != nil {
		t.Fatalf("clean exit did not release the lock: %v", lerr)
	}
	lock.unlock()
}

// TestRunHostDispatcher_RefusesInvalidHostCapacity (#4241): an invalid
// FISHHAWK_HOST_CAPACITY refuses the start with an error naming the variable,
// before the state dir is created or locked. The config is otherwise valid
// and the context pre-cancelled, so without the check the dispatcher would
// tick once, create the state dir and exit cleanly.
func TestRunHostDispatcher_RefusesInvalidHostCapacity(t *testing.T) {
	t.Setenv(hostCapacityEnv, "0")
	dir := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunHostDispatcher(ctx, HostDispatcherConfig{StateDir: dir, AllowedRoots: []string{"/tmp"}, BackendURL: "http://127.0.0.1:1", RunnerBinary: "/bin/true", Log: io.Discard})
	if err == nil || !errors.Is(err, errInvalidHostCapacity) || !strings.Contains(err.Error(), hostCapacityEnv) {
		t.Fatalf("err = %v, want a refusal naming %s", err, hostCapacityEnv)
	}
	if _, serr := os.Stat(dir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("a refused start created the state dir: %v", serr)
	}
}

// --- #5402: per-run refusals, completed-tick health, shared helpers ---------

// hdStatusFileRaw reads status.json as raw JSON, so a test sees exactly what
// the writer put in the FILE (absent keys included).
func hdStatusFileRaw(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, hostDispatchStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode status.json: %v", err)
	}
	return m
}

func hdMustStatus(t *testing.T, dir string) HostDispatcherStatus {
	t.Helper()
	st, err := ReadHostDispatcherStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func hdHasControlRune(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// Item 4: a non-benign refusal is recorded per run in the FILE, the spawned
// run has no entry, and the global last_refusal_* fields are unchanged. The
// writer bounds the detail (S4).
func TestHostDispatchLoop_RunRefusals_RecordedPerRun(t *testing.T) {
	lh := newHDLoopHarness(t)
	spawned := lh.add(0, nil)
	capped := lh.add(time.Minute, nil)
	l := lh.loop(func(c *HostDispatcherConfig) { c.DailyCap = 1 })
	lh.tick(l)
	st := hdMustStatus(t, lh.dir)
	ref, ok := st.RunRefusals[capped.ID]
	if !ok {
		t.Fatalf("status.json run_refusals = %+v; want an entry for the capped run %s", st.RunRefusals, capped.ID)
	}
	if ref.RunID != capped.ID || ref.Code != hdRefuseCapDaily || ref.StageID != capped.Stages[0].ID || !ref.FirstAt.Equal(lh.clock) || !ref.LastAt.Equal(lh.clock) {
		t.Fatalf("entry = %+v; want run %s, %s, stage %s, first_at == last_at == %s", ref, capped.ID, hdRefuseCapDaily, capped.Stages[0].ID, lh.clock)
	}
	if _, ok := st.RunRefusals[spawned.ID]; ok || lh.spawner.n() != 1 {
		t.Fatalf("spawned run has an entry (%v) or spawns = %d; want no entry and one spawn", ok, lh.spawner.n())
	}
	if st.LastRefusalCode != hdRefuseCapDaily || st.LastRefusalRunID != capped.ID || st.LastRefusalStageID != capped.Stages[0].ID {
		t.Fatalf("global last refusal = %q run=%s stage=%s; want %s for %s", st.LastRefusalCode, st.LastRefusalRunID, st.LastRefusalStageID, hdRefuseCapDaily, capped.ID)
	}

	// The writer bound: a long detail with control characters.
	long := strings.Repeat("x", 2500) + "\n\x1b[31m" + strings.Repeat("y", 2490)
	l.record(hostDispatchOutcome{RunID: capped.ID, StageID: capped.Stages[0].ID, Refusal: "marker_refused_by_test", Detail: long}, map[string]bool{})
	l.writeStatus()
	st = hdMustStatus(t, lh.dir)
	d := st.RunRefusals[capped.ID].Detail
	if len(d) == 0 || len(d) > hostDispatchTextMax || hdHasControlRune(d) || !utf8.ValidString(d) {
		t.Fatalf("file detail is %d bytes (control rune %v); want 1..%d bytes, utf8, no control rune", len(d), hdHasControlRune(d), hostDispatchTextMax)
	}
}

// Item 5: a spawn clears the run's entry, a benign outcome clears it (C is
// still a candidate, so only that branch removes it), and a run that is no
// longer a candidate (terminal) is pruned.
func TestHostDispatchLoop_RunRefusals_ClearedAndPruned(t *testing.T) {
	lh := newHDLoopHarness(t)
	later := filepath.Join(lh.root, "later")
	a := lh.add(0, func(r *hdRun) { r.WorkingDir = later })
	b := lh.add(time.Minute, func(r *hdRun) { r.Link = hdLinked("paused") })
	c := lh.add(2*time.Minute, func(r *hdRun) { r.WorkingDir = filepath.Join(lh.root, "never") })
	l := lh.loop(nil)
	lh.tick(l)
	st := hdMustStatus(t, lh.dir)
	if st.RunRefusals[a.ID].Code != hdRefuseWorkingDirAbsent || st.RunRefusals[b.ID].Code != hdRefuseCampaignNotRunning || st.RunRefusals[c.ID].Code != hdRefuseWorkingDirAbsent {
		t.Fatalf("tick 1 run_refusals = %+v; want A and C %s, B %s", st.RunRefusals, hdRefuseWorkingDirAbsent, hdRefuseCampaignNotRunning)
	}
	if err := os.Mkdir(later, 0o755); err != nil {
		t.Fatal(err)
	}
	lh.b.mu.Lock()
	b.State = "succeeded"
	c.AutoDispatch = false // tick 2 reads the benign not_eligible
	lh.b.mu.Unlock()
	lh.clock = lh.clock.Add(30 * time.Second)
	lh.tick(l)
	if lh.spawner.n() != 1 || lh.spawner.spawns[0].Run != a.ID {
		t.Fatalf("tick 2 spawns = %+v; want A spawned", lh.spawner.spawns)
	}
	st = hdMustStatus(t, lh.dir)
	if ref, ok := st.RunRefusals[a.ID]; ok {
		t.Fatalf("A's entry survived its spawn: %+v", ref)
	}
	if ref, ok := st.RunRefusals[b.ID]; ok {
		t.Fatalf("terminal B's entry was not pruned: %+v", ref)
	}
	if ref, ok := st.RunRefusals[c.ID]; ok {
		t.Fatalf("C's entry survived a benign outcome: %+v", ref)
	}
	if _, ok := hdStatusFileRaw(t, lh.dir)["run_refusals"]; ok {
		t.Fatal("status.json still carries a run_refusals key with no entries")
	}
}

// Item 6: a repeated refusal is logged once per run; first_at is kept and
// last_at advances; a code change logs again and resets first_at.
func TestHostDispatchLoop_RepeatedRefusalLoggedOnce(t *testing.T) {
	lh := newHDLoopHarness(t)
	later := filepath.Join(lh.root, "later")
	r := lh.add(0, func(r *hdRun) { r.WorkingDir = later })
	var logBuf bytes.Buffer
	l := lh.loop(func(c *HostDispatcherConfig) { c.Log = &logBuf })
	lines := func() int { return strings.Count(logBuf.String(), "refused run="+r.ID) }
	t0 := lh.clock
	for i := 0; i < 3; i++ {
		lh.tick(l)
		lh.clock = lh.clock.Add(30 * time.Second)
	}
	if n := lines(); n != 1 {
		t.Fatalf("refused lines after 3 identical refusals = %d; want 1:\n%s", n, logBuf.String())
	}
	ref := hdMustStatus(t, lh.dir).RunRefusals[r.ID]
	if !ref.FirstAt.Equal(t0) || !ref.LastAt.Equal(t0.Add(60*time.Second)) {
		t.Fatalf("entry = %+v; want first_at %s kept and last_at %s", ref, t0, t0.Add(60*time.Second))
	}
	if err := os.Mkdir(later, 0o755); err != nil {
		t.Fatal(err)
	}
	lh.b.mu.Lock()
	r.Link = hdLinked("paused")
	lh.b.mu.Unlock()
	lh.tick(l)
	if n := lines(); n != 2 {
		t.Fatalf("refused lines after a code change = %d; want 2:\n%s", n, logBuf.String())
	}
	ref = hdMustStatus(t, lh.dir).RunRefusals[r.ID]
	if ref.Code != hdRefuseCampaignNotRunning || !ref.FirstAt.Equal(lh.clock) {
		t.Fatalf("entry after the change = %+v; want %s with first_at reset to %s", ref, hdRefuseCampaignNotRunning, lh.clock)
	}
}

// Item 7 (R2): every non-paused early return records last_tick_failure, so a
// dispatcher whose ticks fail reads failing rather than live; a paused tick
// records none; a completed tick clears it.
func TestTick_EarlyReturnRecordsTickFailure(t *testing.T) {
	rows := []struct {
		code  string
		fault func(lh *hdLoopHarness, l *hostDispatchLoop)
	}{
		{hdRefuseLockLost, func(_ *hdLoopHarness, l *hostDispatchLoop) { l.lockHeld = func() bool { return false } }},
		{hdRefuseLedgerRead, func(lh *hdLoopHarness, _ *hostDispatchLoop) {
			path := filepath.Join(lh.dir, hostDispatchLedgerFile)
			target := filepath.Join(lh.t.TempDir(), "ledger.json")
			if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
				lh.t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				lh.t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				lh.t.Fatal(err)
			}
		}},
		{hdRefuseLedgerWrite, func(_ *hdLoopHarness, l *hostDispatchLoop) {
			l.writeFile = func(dir, name string, data []byte) error {
				if name == hostDispatchLedgerFile {
					return errors.New("disk full")
				}
				return writeHostDispatchFile(dir, name, data)
			}
		}},
		{hdRefuseListFailed, func(lh *hdLoopHarness, _ *hostDispatchLoop) {
			lh.mu.Lock()
			lh.listCode = 500
			lh.mu.Unlock()
		}},
	}
	for _, row := range rows {
		t.Run(row.code, func(t *testing.T) {
			lh := newHDLoopHarness(t)
			l := lh.loop(nil)
			t0 := lh.clock
			lh.tick(l)
			t1 := t0.Add(30 * time.Second)
			lh.clock = t1
			row.fault(lh, l)
			_ = l.tick(context.Background()) // state_dir_lock_lost returns its error
			st := hdMustStatus(t, lh.dir)
			if !st.LastTickAt.Equal(t1) || !st.LastCompleteTickAt.Equal(t0) {
				t.Fatalf("last_tick_at=%s last_complete_tick_at=%s; want %s and %s", st.LastTickAt, st.LastCompleteTickAt, t1, t0)
			}
			if st.LastTickFailure == nil || st.LastTickFailure.Code != row.code || !st.LastTickFailure.At.Equal(t1) {
				t.Fatalf("last_tick_failure = %+v; want {%s %s}", st.LastTickFailure, row.code, t1)
			}
			if got := st.State(t1); got != "failing" {
				t.Fatalf("State = %q; want failing", got)
			}
			if row.code != hdRefuseListFailed {
				return
			}
			// Recovery: the fault clears and a completed tick drops the failure.
			lh.mu.Lock()
			lh.listCode = 0
			lh.mu.Unlock()
			t2 := t1.Add(30 * time.Second)
			lh.clock = t2
			lh.tick(l)
			if _, ok := hdStatusFileRaw(t, lh.dir)["last_tick_failure"]; ok {
				t.Fatal("status.json still carries last_tick_failure after a completed tick")
			}
			if st := hdMustStatus(t, lh.dir); !st.LastCompleteTickAt.Equal(t2) || st.State(t2) != "live" {
				t.Fatalf("after recovery last_complete_tick_at=%s state=%s; want %s, live", st.LastCompleteTickAt, st.State(t2), t2)
			}
		})
	}
	t.Run("paused", func(t *testing.T) {
		lh := newHDLoopHarness(t)
		l := lh.loop(nil)
		t0 := lh.clock
		lh.tick(l)
		t1 := t0.Add(30 * time.Second)
		lh.clock = t1
		if err := SetHostDispatcherPaused(lh.dir, true); err != nil {
			t.Fatal(err)
		}
		lh.tick(l)
		st := hdMustStatus(t, lh.dir)
		if !st.Paused || st.LastTickFailure != nil || !st.LastCompleteTickAt.Equal(t0) || st.State(t1) != "paused" {
			t.Fatalf("paused tick: paused=%v failure=%+v last_complete=%s state=%s; want true, nil, %s, paused", st.Paused, st.LastTickFailure, st.LastCompleteTickAt, st.State(t1), t0)
		}
	})
}

// Item 8: State's order and the S3 unknown-zero rule, on constructed statuses.
func TestHostDispatcherStatus_State(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	iv := 30 * time.Second
	ago := func(n int) time.Time { return now.Add(-time.Duration(n) * iv) }
	fail := func(at time.Time) *hostDispatchTickFailure {
		return &hostDispatchTickFailure{Code: hdRefuseListFailed, At: at}
	}
	rows := []struct {
		name string
		st   HostDispatcherStatus
		want string
	}{
		{"live", HostDispatcherStatus{LastTickAt: ago(0), LastCompleteTickAt: ago(0)}, "live"},
		{"stale last tick", HostDispatcherStatus{LastTickAt: ago(4), LastCompleteTickAt: ago(4)}, "stale"},
		{"paused, old completed pass", HostDispatcherStatus{LastTickAt: ago(0), LastCompleteTickAt: ago(10), Paused: true}, "paused"},
		{"failure newer than last complete", HostDispatcherStatus{LastTickAt: ago(0), LastCompleteTickAt: ago(1), LastTickFailure: fail(ago(0))}, "failing"},
		{"failure older than last complete", HostDispatcherStatus{LastTickAt: ago(0), LastCompleteTickAt: ago(0), LastTickFailure: fail(ago(1))}, "live"},
		{"completed pass stale, no failure", HostDispatcherStatus{LastTickAt: ago(0), LastCompleteTickAt: ago(4)}, "stale"},
		{"S3 zero complete, fresh tick", HostDispatcherStatus{LastTickAt: ago(0)}, "live"},
		{"S3 zero complete, old tick", HostDispatcherStatus{LastTickAt: ago(4)}, "stale"},
		{"S3 zero complete, fresh failure", HostDispatcherStatus{LastTickAt: ago(0), LastTickFailure: fail(ago(0))}, "failing"},
	}
	for _, row := range rows {
		row.st.IntervalSeconds = iv.Seconds()
		if got := row.st.State(now); got != row.want {
			t.Errorf("%s: State = %q; want %q", row.name, got, row.want)
		}
	}
}

// Item 14 (S4): the shared text bound.
func TestBoundHostDispatchText(t *testing.T) {
	if got := boundHostDispatchText(strings.Repeat("a", 5000)); len(got) != hostDispatchTextMax {
		t.Errorf("5000-byte ASCII: len = %d; want %d", len(got), hostDispatchTextMax)
	}
	// 299 ASCII bytes then a 3-byte rune: the 300-byte cut falls inside it.
	split := strings.Repeat("a", 299) + "€" + "tail"
	if got := boundHostDispatchText(split); len(got) > hostDispatchTextMax || !utf8.ValidString(got) || got != strings.Repeat("a", 299) {
		t.Errorf("mid-rune cut: %d bytes valid=%v; want the 299-byte prefix", len(got), utf8.ValidString(got))
	}
	rows := map[string]string{
		"a\x1b[31mb\nc\u0085d": "a[31mbcd",
		"a\xffb":               "ab",
		"short clean text":     "short clean text",
		"tab\there\x7f":        "tabhere",
	}
	for in, want := range rows {
		if got := boundHostDispatchText(in); got != want {
			t.Errorf("boundHostDispatchText(%q) = %q; want %q", in, got, want)
		}
	}
}
