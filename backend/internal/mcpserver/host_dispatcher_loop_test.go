package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
		lh.mu.Unlock()
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

// F7(e): the state-dir pid lock refuses a second dispatcher, takes over a
// dead holder's lock, and a dispatcher whose lock was taken stops.
func TestStateDirLock_SecondDispatcherRefused(t *testing.T) {
	dir := t.TempDir()
	unlock, err := acquireHostDispatchLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireHostDispatchLock(dir); err == nil || !strings.Contains(err.Error(), hdStateDirLocked) {
		t.Fatalf("second lock: err = %v; want %s", err, hdStateDirLocked)
	}
	assertMode(t, filepath.Join(dir, hostDispatchLockFile), 0o600)
	unlock()
	if _, err := os.Stat(filepath.Join(dir, hostDispatchLockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlock left the lock file: %v", err)
	}

	// A dead holder's lock is taken over.
	if err := os.WriteFile(filepath.Join(dir, hostDispatchLockFile), []byte("2147483646"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err = acquireHostDispatchLock(dir)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	defer unlock()

	// A dispatcher whose lock now names another pid stops at its next tick.
	lh := newHDLoopHarness(t)
	lh.dir = dir
	l := lh.loop(nil)
	lockPath := filepath.Join(dir, hostDispatchLockFile)
	pid := os.Getpid()
	l.lockHeld = func() bool { h, err := readHostDispatchLock(lockPath); return err == nil && h == pid }
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(pid+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	lh.add(0, nil)
	if err := l.tick(context.Background()); err == nil || !strings.Contains(err.Error(), hdRefuseLockLost) {
		t.Fatalf("tick with a lost lock: err = %v; want %s", err, hdRefuseLockLost)
	}
	if lh.b.count(func(hdRequest) bool { return true }) != 0 {
		t.Fatal("a dispatcher that lost its lock still made requests")
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
	if _, err := os.Stat(filepath.Join(dir, hostDispatchLockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean exit left the lock: %v", err)
	}
}
