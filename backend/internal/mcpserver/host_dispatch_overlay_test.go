package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// host_dispatch_overlay_test.go pins the stdio host-dispatch status overlay
// (#5402). Every test that can reach the TMPDIR rung sets TMPDIR with
// t.Setenv (so none is parallel) and injects getwd, so no test reads the
// host's real dispatcher state.

var hdoNow = time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

const hdoBackendNext = "The host dispatcher spawns this stage on its next tick; no operator action is needed."

// hdoStatus is a fresh, healthy post-#5402 status at hdoNow.
func hdoStatus() HostDispatcherStatus {
	return HostDispatcherStatus{
		PID: 7, Host: "h", LastTickAt: hdoNow.Add(-10 * time.Second), LastCompleteTickAt: hdoNow.Add(-10 * time.Second),
		IntervalSeconds: 30, SpawnsDay: hdoNow.Format(time.DateOnly),
	}
}

// hdoWrite writes v (json.Marshal, bypassing the dispatcher's writer) as
// dir/status.json, creating dir 0700.
func hdoWrite(t *testing.T, dir string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	hdoWriteRaw(t, dir, raw)
}

func hdoWriteRaw(t *testing.T, dir string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hostDispatchStatusFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// hdoRuns is a fake backend serving GET /v0/runs/{id} with a per-run
// working_dir (or a 500), counting reads.
type hdoRuns struct {
	mu    sync.Mutex
	dirs  map[string]string
	fail  map[string]bool
	reads int
	srv   *httptest.Server
}

func newHDORuns(t *testing.T) *hdoRuns {
	t.Helper()
	b := &hdoRuns{dirs: map[string]string{}, fail: map[string]bool{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := strings.TrimPrefix(req.URL.Path, "/v0/runs/")
		b.mu.Lock()
		b.reads++
		dir, fail := b.dirs[id], b.fail[id]
		b.mu.Unlock()
		if req.Method != http.MethodGet || strings.Contains(id, "/") || fail {
			writeJSON(w, http.StatusInternalServerError, apiErrBody("internal", "run read failed"))
			return
		}
		body, _ := json.Marshal(Run{ID: id, State: "running", RunnerKind: "local", WorkingDir: dir, AutoDispatch: true})
		writeJSON(w, http.StatusOK, string(body))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *hdoRuns) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

// hdoResolver is an enabled overlay resolver: env, cwd and clock injected.
func hdoResolver(env map[string]string, cwd string, b *hdoRuns) *runResolver {
	r := &runResolver{
		localHostRungs:  true,
		getenv:          envFuncFromMap(env),
		getwd:           func() (string, error) { return cwd, nil },
		hostDispatchNow: func() time.Time { return hdoNow },
	}
	if b != nil {
		r.api = newAPIClient(config{backendURL: b.srv.URL, apiToken: "tok-test"})
	}
	return r
}

func hdoItem(runID, stageID string) CampaignItem {
	return CampaignItem{IssueRef: "issue:" + runID[:4], State: "running", RunID: runID, Awaiting: &campaignItemAwaiting{
		Kind: hostDispatchAwaitKind, Reason: hostDispatchReasonDispatch, RunID: runID, StageID: stageID,
		StageType: "implement", StageState: "awaiting_host_dispatch", NextStep: hdoBackendNext,
	}}
}

// hdoOverlayOne runs the campaign overlay over one item and returns its block.
func hdoOverlayOne(t *testing.T, r *runResolver, runID, stageID string) *campaignItemAwaiting {
	t.Helper()
	items := []CampaignItem{hdoItem(runID, stageID)}
	r.overlayCampaignHostDispatch(context.Background(), items)
	if items[0].Awaiting.HostDispatch == nil {
		t.Fatalf("no host_dispatch block on %+v", items[0].Awaiting)
	}
	return items[0].Awaiting
}

func hdoEmptyTMPDIR(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	return filepath.Join(tmp, hostDispatchDefaultStateDirName)
}

func hdoContainsAll(t *testing.T, label, s string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("%s = %q; want it to contain %q", label, s, w)
		}
	}
}

func hdoContainsNone(t *testing.T, label, s string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(s, b) {
			t.Errorf("%s = %q; must not contain %q", label, s, b)
		}
	}
}

func TestHostDispatchOverlay_Modes(t *testing.T) {
	runID, stageID, otherStage := uuid.NewString(), uuid.NewString(), uuid.NewString()
	interval := 30 * time.Second
	entry := func(code, stage, detail string) map[string]hostDispatchRunRefusal {
		return map[string]hostDispatchRunRefusal{runID: {RunID: runID, StageID: stage, Code: code, Detail: detail,
			FirstAt: hdoNow.Add(-5 * time.Minute), LastAt: hdoNow.Add(-10 * time.Second)}}
	}
	with := func(mut func(*HostDispatcherStatus)) HostDispatcherStatus {
		st := hdoStatus()
		mut(&st)
		return st
	}
	failing := func(code string) func(*HostDispatcherStatus) {
		return func(s *HostDispatcherStatus) {
			s.LastTickAt = hdoNow.Add(-5 * time.Second)
			s.LastTickFailure = &hostDispatchTickFailure{Code: code, At: hdoNow.Add(-5 * time.Second)}
		}
	}
	const noAction = "no operator action is needed"
	type row struct {
		name       string
		st         HostDispatcherStatus
		raw        []byte // written verbatim instead of st when set
		symlink    bool   // status.json is a symlink to a valid file
		kind       string
		reason     string
		state      string
		contains   []string
		notContain []string
		check      func(t *testing.T, blk *campaignItemAwaiting, dir string)
	}
	rows := []row{
		{name: "a live, no entry", st: hdoStatus(), kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateLive,
			check: func(t *testing.T, blk *campaignItemAwaiting, _ string) {
				if blk.NextStep != hdoBackendNext {
					t.Errorf("live next_step = %q; want the backend's text unchanged", blk.NextStep)
				}
			}},
		{name: "c stale last tick", st: with(func(s *HostDispatcherStatus) { s.LastTickAt = hdoNow.Add(-4 * interval) }),
			kind: "human", reason: hostDispatchReasonAbsent, state: hostDispatchStateStale,
			contains: []string{"was last written at " + hdoNow.Add(-4*interval).Format(time.RFC3339), "fishhawk-mcp host-dispatch --status --state-dir"}},
		{name: "d corrupt json", raw: []byte("{not json"), kind: hostDispatchAwaitKind, reason: hostDispatchReasonUnverified, state: hostDispatchStateUnverified,
			check: func(t *testing.T, blk *campaignItemAwaiting, dir string) {
				hdoContainsAll(t, "next_step", blk.NextStep, "fishhawk-mcp host-dispatch --status --state-dir "+dir)
			}},
		{name: "d symlinked status.json", symlink: true, kind: hostDispatchAwaitKind, reason: hostDispatchReasonUnverified, state: hostDispatchStateUnverified,
			check: func(t *testing.T, blk *campaignItemAwaiting, dir string) {
				hdoContainsAll(t, "next_step", blk.NextStep, "fishhawk-mcp host-dispatch --status --state-dir "+dir, "symlink")
			}},
		{name: "e paused", st: with(func(s *HostDispatcherStatus) { s.Paused = true }), kind: "human", reason: hostDispatchReasonPaused, state: hostDispatchStatePaused,
			contains: []string{"--resume --state-dir", "fishhawk_dispatch_stage"}},
		{name: "e paused, pause check failed this tick", st: with(func(s *HostDispatcherStatus) {
			s.Paused = true
			s.LastRefusalCode, s.LastRefusalAt = hdRefusePauseCheck, s.LastTickAt
		}), kind: "human", reason: hostDispatchReasonPaused, state: hostDispatchStatePaused, contains: []string{"pause check failed"}, notContain: []string{"--resume"}},
		// F2: an OLD pause_check_failed plus a deliberate pause reads the plain
		// --resume text.
		{name: "e F2 old pause check failure, deliberate pause", st: with(func(s *HostDispatcherStatus) {
			s.Paused = true
			s.LastRefusalCode, s.LastRefusalAt = hdRefusePauseCheck, s.LastTickAt.Add(-10*time.Minute)
		}), kind: "human", reason: hostDispatchReasonPaused, state: hostDispatchStatePaused, contains: []string{"--resume --state-dir"}, notContain: []string{"pause check failed"}},
		{name: "g completed pass stale", st: with(func(s *HostDispatcherStatus) { s.LastCompleteTickAt = hdoNow.Add(-4 * interval) }),
			kind: "human", reason: hostDispatchReasonAbsent, state: hostDispatchStateStale, contains: []string{"no completed candidate pass since " + hdoNow.Add(-4*interval).Format(time.RFC3339)}},
		{name: "h global daily cap", st: with(func(s *HostDispatcherStatus) { s.DailyCap, s.SpawnsToday = 2, 2 }),
			kind: "human", reason: hostDispatchReasonCapped, state: hostDispatchStateCapped,
			contains: []string{"--daily-cap 2", "fishhawk_dispatch_stage", "2026-11-21T00:00:00Z"}, notContain: []string{noAction}},
		{name: "i daily cap from yesterday", st: with(func(s *HostDispatcherStatus) {
			s.DailyCap, s.SpawnsToday, s.SpawnsDay = 2, 2, hdoNow.Add(-24*time.Hour).Format(time.DateOnly)
		}), kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateLive},
		{name: "j entry working_dir_absent", st: with(func(s *HostDispatcherStatus) { s.RunRefusals = entry(hdRefuseWorkingDirAbsent, stageID, "dir gone") }),
			kind: "human", reason: hostDispatchReasonRefused, state: hostDispatchStateRefusing,
			contains: []string{"The host dispatcher refuses this stage: working_dir_absent (dir gone), since", "fishhawk_dispatch_stage"}},
		{name: "k entry with empty stage id", st: with(func(s *HostDispatcherStatus) { s.RunRefusals = entry(hdRefuseWorkingDirUnbound, "", "") }),
			kind: "human", reason: hostDispatchReasonRefused, state: hostDispatchStateRefusing, contains: []string{hdRefuseWorkingDirUnbound}},
		{name: "l S5b stage mismatch", st: with(func(s *HostDispatcherStatus) { s.RunRefusals = entry(hdRefuseWorkingDirAbsent, otherStage, "") }),
			kind: hostDispatchAwaitKind, reason: hostDispatchReasonUnverified, state: hostDispatchStateUnverified,
			contains: []string{stageID, otherStage, "fishhawk-mcp host-dispatch --status"}, notContain: []string{noAction},
			check: func(t *testing.T, blk *campaignItemAwaiting, _ string) {
				if blk.HostDispatch.RefusalStageID != otherStage {
					t.Errorf("refusal_stage_id = %q; want %q", blk.HostDispatch.RefusalStageID, otherStage)
				}
			}},
		{name: "m entry concurrency_slot_queued", st: with(func(s *HostDispatcherStatus) { s.RunRefusals = entry(hdRefuseSlotQueued, stageID, "") }),
			kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateWaiting, contains: []string{hdRefuseSlotQueued, noAction}},
		{name: "n entry cap_reached:daily", st: with(func(s *HostDispatcherStatus) {
			s.DailyCap, s.SpawnsToday = 1, 1
			s.RunRefusals = entry(hdRefuseCapDaily, stageID, "")
		}), kind: "human", reason: hostDispatchReasonCapped, state: hostDispatchStateCapped},
		{name: "o entry paused (mid-tick)", st: with(func(s *HostDispatcherStatus) { s.RunRefusals = entry(hdRefusePaused, stageID, "") }),
			kind: "human", reason: hostDispatchReasonPaused, state: hostDispatchStatePaused},
		{name: "o entry pause_check_failed (mid-tick)", st: with(func(s *HostDispatcherStatus) { s.RunRefusals = entry(hdRefusePauseCheck, stageID, "") }),
			kind: "human", reason: hostDispatchReasonPaused, state: hostDispatchStatePaused},
		{name: "p entry campaign_not_running", st: with(func(s *HostDispatcherStatus) {
			s.RunRefusals = entry(hdRefuseCampaignNotRunning, stageID, "campaign c is paused")
		}), kind: "human", reason: hostDispatchReasonRefused, state: hostDispatchStateHeld,
			contains: []string{"Do not dispatch past the hold"}, notContain: []string{"fishhawk_dispatch_stage"},
			check: func(t *testing.T, blk *campaignItemAwaiting, _ string) {
				if !strings.HasPrefix(blk.NextStep, "The linked campaign is not running (campaign c is paused)") {
					t.Errorf("next_step = %q; want the cause first", blk.NextStep)
				}
			}},
		{name: "p entry campaign_dispatch_held", st: with(func(s *HostDispatcherStatus) {
			s.RunRefusals = entry(hostDispatchCampaignHeldCode, stageID, "hotfix #9")
		}), kind: "human", reason: hostDispatchReasonRefused, state: hostDispatchStateHeld,
			contains: []string{"Do not dispatch past the hold"}, notContain: []string{"fishhawk_dispatch_stage"},
			check: func(t *testing.T, blk *campaignItemAwaiting, _ string) {
				if !strings.HasPrefix(blk.NextStep, "A campaign hotfix hold is active (hotfix #9)") {
					t.Errorf("next_step = %q; want the cause first", blk.NextStep)
				}
			}},
		{name: "q entry for another run only", st: with(func(s *HostDispatcherStatus) {
			other := uuid.NewString()
			s.RunRefusals = map[string]hostDispatchRunRefusal{other: {RunID: other, Code: hdRefuseWorkingDirAbsent}}
		}), kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateLive},
		{name: "r stale outranks paused", st: with(func(s *HostDispatcherStatus) { s.LastTickAt, s.Paused = hdoNow.Add(-4*interval), true }),
			kind: "human", reason: hostDispatchReasonAbsent, state: hostDispatchStateStale},
		{name: "r paused outranks an entry", st: with(func(s *HostDispatcherStatus) {
			s.Paused = true
			s.RunRefusals = entry(hdRefuseWorkingDirAbsent, stageID, "")
		}), kind: "human", reason: hostDispatchReasonPaused, state: hostDispatchStatePaused},
		{name: "r failing outranks an entry", st: with(func(s *HostDispatcherStatus) {
			failing(hdRefuseListFailed)(s)
			s.RunRefusals = entry(hdRefuseWorkingDirAbsent, stageID, "")
		}), kind: "human", reason: hostDispatchReasonRefused, state: hostDispatchStateRefusing,
			check: func(t *testing.T, blk *campaignItemAwaiting, _ string) {
				if blk.HostDispatch.TickFailureCode != hdRefuseListFailed {
					t.Errorf("tick_failure_code = %q; want %s", blk.HostDispatch.TickFailureCode, hdRefuseListFailed)
				}
				hdoContainsAll(t, "next_step", blk.NextStep, "last tick failed with "+hdRefuseListFailed)
			}},
		{name: "r refusing entry outranks the global cap", st: with(func(s *HostDispatcherStatus) {
			s.DailyCap, s.SpawnsToday = 2, 2
			s.RunRefusals = entry(hdRefuseWorkingDirAbsent, stageID, "")
		}), kind: "human", reason: hostDispatchReasonRefused, state: hostDispatchStateRefusing},
		{name: "r global cap outranks a waiting entry", st: with(func(s *HostDispatcherStatus) {
			s.DailyCap, s.SpawnsToday = 2, 2
			s.RunRefusals = entry(hdRefuseSlotQueued, stageID, "")
		}), kind: "human", reason: hostDispatchReasonCapped, state: hostDispatchStateCapped},
		{name: "r global cap outranks a stage mismatch", st: with(func(s *HostDispatcherStatus) {
			s.DailyCap, s.SpawnsToday = 2, 2
			s.RunRefusals = entry(hdRefuseWorkingDirAbsent, otherStage, "")
		}), kind: "human", reason: hostDispatchReasonCapped, state: hostDispatchStateCapped},
		// Freshness bounds (27e34411): a future-dated stamp never ages out, and
		// a huge interval_seconds would stretch the 3x window without the clamp.
		{name: "t future-dated last tick reads stale", st: with(func(s *HostDispatcherStatus) { s.LastTickAt = hdoNow.Add(2 * time.Minute) }),
			kind: "human", reason: hostDispatchReasonAbsent, state: hostDispatchStateStale,
			contains: []string{"is future-dated", "last_tick_at " + hdoNow.Add(2*time.Minute).Format(time.RFC3339)}, notContain: []string{noAction},
			check: func(t *testing.T, blk *campaignItemAwaiting, _ string) {
				if !strings.HasPrefix(blk.HostDispatch.Detail, "future-dated") {
					t.Errorf("detail = %q; want it to start with future-dated", blk.HostDispatch.Detail)
				}
			}},
		{name: "t future-dated last complete tick reads stale", st: with(func(s *HostDispatcherStatus) { s.LastCompleteTickAt = hdoNow.Add(2 * time.Minute) }),
			kind: "human", reason: hostDispatchReasonAbsent, state: hostDispatchStateStale, contains: []string{"is future-dated"},
			check: func(t *testing.T, blk *campaignItemAwaiting, _ string) {
				if !strings.HasPrefix(blk.HostDispatch.Detail, "future-dated") {
					t.Errorf("detail = %q; want it to start with future-dated", blk.HostDispatch.Detail)
				}
			}},
		{name: "t a stamp inside the 60s skew reads live", st: with(func(s *HostDispatcherStatus) { s.LastTickAt = hdoNow.Add(30 * time.Second) }),
			kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateLive},
		{name: "t interval_seconds 1e9 is clamped to 300", st: with(func(s *HostDispatcherStatus) {
			s.IntervalSeconds = 1e9
			s.LastTickAt, s.LastCompleteTickAt = hdoNow.Add(-20*time.Minute), hdoNow.Add(-20*time.Minute)
		}), kind: "human", reason: hostDispatchReasonAbsent, state: hostDispatchStateStale, contains: []string{"was last written at"}},
		{name: "t the clamped 300s interval still allows a 15-minute window", st: with(func(s *HostDispatcherStatus) {
			s.IntervalSeconds = 1e9
			s.LastTickAt, s.LastCompleteTickAt = hdoNow.Add(-14*time.Minute), hdoNow.Add(-14*time.Minute)
		}), kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateLive},
		{name: "t interval_seconds below 1 is clamped to 1", st: with(func(s *HostDispatcherStatus) {
			s.IntervalSeconds = 0.001
			s.LastTickAt, s.LastCompleteTickAt = hdoNow.Add(-2*time.Second), hdoNow.Add(-2*time.Second)
		}), kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateLive},
		{name: "s pre-#5402 status reads live", st: HostDispatcherStatus{PID: 7, LastTickAt: hdoNow.Add(-60 * time.Second), IntervalSeconds: 30, SpawnsToday: 5, SpawnsDay: hdoNow.Format(time.DateOnly)},
			kind: hostDispatchAwaitKind, reason: hostDispatchReasonDispatch, state: hostDispatchStateLive},
	}
	// f: one row per dispatcher-wide failing early return (R2).
	for _, code := range []string{hdRefuseLedgerRead, hdRefuseLedgerWrite, hdRefuseListFailed, hdRefuseLockLost} {
		rows = append(rows, row{name: "f dispatcher-wide " + code, st: with(failing(code)), kind: "human", reason: hostDispatchReasonRefused,
			state: hostDispatchStateRefusing, contains: []string{code, "fishhawk_dispatch_stage"}, notContain: []string{noAction}})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			hdoEmptyTMPDIR(t)
			dir := filepath.Join(t.TempDir(), "state")
			switch {
			case tc.symlink:
				real := filepath.Join(t.TempDir(), "real.json")
				raw, _ := json.Marshal(hdoStatus())
				if err := os.WriteFile(real, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, filepath.Join(dir, hostDispatchStatusFile)); err != nil {
					t.Fatal(err)
				}
			case tc.raw != nil:
				hdoWriteRaw(t, dir, tc.raw)
			default:
				hdoWrite(t, dir, tc.st)
			}
			r := hdoResolver(map[string]string{hostDispatchStateDirEnv: dir}, t.TempDir(), nil)
			blk := hdoOverlayOne(t, r, runID, stageID)
			if blk.Kind != tc.kind || blk.Reason != tc.reason || blk.HostDispatch.State != tc.state {
				t.Fatalf("awaiting = %s/%s state %q; want %s/%s state %q (next_step %q)", blk.Kind, blk.Reason, blk.HostDispatch.State, tc.kind, tc.reason, tc.state, blk.NextStep)
			}
			if tc.state != hostDispatchStateLive && blk.NextStep == hdoBackendNext {
				t.Errorf("next_step is still the backend's 'spawns on its next tick' text")
			}
			hdoContainsAll(t, "next_step", blk.NextStep, tc.contains...)
			hdoContainsNone(t, "next_step", blk.NextStep, tc.notContain...)
			if tc.check != nil {
				tc.check(t, blk, dir)
			}
		})
	}

	// b: no status on any rung (env unset): absent, naming every path tried,
	// never claiming the dispatcher is down (R3).
	t.Run("b absent on every rung", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		cwd, wd := t.TempDir(), t.TempDir()
		b := newHDORuns(t)
		b.dirs[runID] = wd
		r := hdoResolver(nil, cwd, b)
		blk := hdoOverlayOne(t, r, runID, stageID)
		if blk.Kind != "human" || blk.Reason != hostDispatchReasonAbsent || blk.HostDispatch.State != hostDispatchStateAbsent {
			t.Fatalf("awaiting = %+v; want human/host_dispatcher_absent", blk)
		}
		hdoContainsAll(t, "next_step", blk.NextStep, "No host dispatcher status file found at",
			filepath.Join(cwd, hostDispatchCheckoutStateDir), filepath.Join(wd, hostDispatchCheckoutStateDir), tmpRung,
			"set FISHHAWK_HOST_DISPATCH_STATE_DIR in this MCP server's environment")
		hdoContainsNone(t, "next_step", blk.NextStep, "not running", "down")
	})
}

// TestHostDispatchOverlay_StateDirLadder pins the discovery ladder: env
// (exclusive), cwd (no run read), each run's working_dir, then TMPDIR; and F4,
// a stale hit does not end the walk.
func TestHostDispatchOverlay_StateDirLadder(t *testing.T) {
	paused := hdoStatus()
	paused.Paused = true
	stale := hdoStatus()
	stale.LastTickAt = hdoNow.Add(-10 * time.Minute)
	runID, stageID := uuid.NewString(), uuid.NewString()
	rung := func(root string) string { return filepath.Join(root, hostDispatchCheckoutStateDir) }

	t.Run("env set is exclusive", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		hdoWrite(t, tmpRung, paused)
		env, cwd, wd := filepath.Join(t.TempDir(), "env"), t.TempDir(), t.TempDir()
		hdoWrite(t, env, hdoStatus())
		hdoWrite(t, rung(cwd), paused)
		hdoWrite(t, rung(wd), paused)
		b := newHDORuns(t)
		b.dirs[runID] = wd
		blk := hdoOverlayOne(t, hdoResolver(map[string]string{hostDispatchStateDirEnv: env}, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStateLive || b.count() != 0 {
			t.Fatalf("state %q, reads %d; want the env dir's live status and zero run reads", blk.HostDispatch.State, b.count())
		}
		empty := filepath.Join(t.TempDir(), "empty")
		blk = hdoOverlayOne(t, hdoResolver(map[string]string{hostDispatchStateDirEnv: empty}, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStateAbsent || len(blk.HostDispatch.PathsTried) != 1 || blk.HostDispatch.PathsTried[0] != empty {
			t.Fatalf("empty env dir: state %q paths %v; want absent naming only %s", blk.HostDispatch.State, blk.HostDispatch.PathsTried, empty)
		}
	})
	t.Run("cwd rung hit needs zero run reads", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd, wd := t.TempDir(), t.TempDir()
		hdoWrite(t, rung(cwd), paused)
		b := newHDORuns(t)
		b.dirs[runID] = wd
		blk := hdoOverlayOne(t, hdoResolver(nil, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStatePaused || b.count() != 0 {
			t.Fatalf("state %q, reads %d; want paused from the cwd rung with zero run reads", blk.HostDispatch.State, b.count())
		}
	})
	t.Run("working_dir rung when cwd misses", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd, wd := t.TempDir(), t.TempDir()
		hdoWrite(t, rung(wd), paused)
		b := newHDORuns(t)
		b.dirs[runID] = wd
		blk := hdoOverlayOne(t, hdoResolver(nil, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStatePaused || b.count() != 1 {
			t.Fatalf("state %q, reads %d; want paused from the working_dir rung after one read", blk.HostDispatch.State, b.count())
		}
	})
	t.Run("R3 multi-root: another checkout misses, the dispatcher's cwd finds it", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		cwd, other := t.TempDir(), t.TempDir()
		b := newHDORuns(t)
		b.dirs[runID] = other
		blk := hdoOverlayOne(t, hdoResolver(nil, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStateAbsent {
			t.Fatalf("state %q; want absent", blk.HostDispatch.State)
		}
		hdoContainsAll(t, "next_step", blk.NextStep, rung(cwd), rung(other), tmpRung)
		hdoWrite(t, rung(cwd), paused)
		blk = hdoOverlayOne(t, hdoResolver(nil, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStatePaused {
			t.Fatalf("with the dispatcher's checkout as cwd: state %q; want paused", blk.HostDispatch.State)
		}
	})
	t.Run("relative or empty working_dir skips that rung", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		cwd := t.TempDir()
		relRun, emptyRun := uuid.NewString(), uuid.NewString()
		b := newHDORuns(t)
		b.dirs[relRun] = "rel/checkout"
		b.dirs[emptyRun] = ""
		items := []CampaignItem{hdoItem(relRun, stageID), hdoItem(emptyRun, stageID)}
		hdoResolver(nil, cwd, b).overlayCampaignHostDispatch(context.Background(), items)
		got := items[0].Awaiting.HostDispatch.PathsTried
		want := []string{rung(cwd), tmpRung}
		if fmt.Sprint(got) != fmt.Sprint(want) || b.count() != 2 {
			t.Fatalf("paths_tried %v, reads %d; want %v after two reads", got, b.count(), want)
		}
	})
	t.Run("TMPDIR rung is found last", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		hdoWrite(t, tmpRung, paused)
		cwd, wd := t.TempDir(), t.TempDir()
		b := newHDORuns(t)
		b.dirs[runID] = wd
		blk := hdoOverlayOne(t, hdoResolver(nil, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStatePaused || blk.HostDispatch.StatusPath != filepath.Join(tmpRung, hostDispatchStatusFile) {
			t.Fatalf("state %q path %q; want paused from %s", blk.HostDispatch.State, blk.HostDispatch.StatusPath, tmpRung)
		}
	})
	// F4: a stale hit does not end the walk; the first non-stale trusted
	// status wins, and when every hit is stale the first is reported.
	t.Run("F4 walk continues past a stale hit", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd, wd := t.TempDir(), t.TempDir()
		hdoWrite(t, rung(cwd), stale)
		hdoWrite(t, rung(wd), paused)
		b := newHDORuns(t)
		b.dirs[runID] = wd
		blk := hdoOverlayOne(t, hdoResolver(nil, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStatePaused || blk.HostDispatch.StatusPath != filepath.Join(rung(wd), hostDispatchStatusFile) {
			t.Fatalf("state %q path %q; want paused from the working_dir rung past the stale cwd hit", blk.HostDispatch.State, blk.HostDispatch.StatusPath)
		}
	})
	t.Run("F4 every hit stale reports the first", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		cwd, wd := t.TempDir(), t.TempDir()
		hdoWrite(t, rung(cwd), stale)
		older := stale
		older.LastTickAt = hdoNow.Add(-time.Hour)
		hdoWrite(t, tmpRung, older)
		b := newHDORuns(t)
		b.dirs[runID] = wd
		blk := hdoOverlayOne(t, hdoResolver(nil, cwd, b), runID, stageID)
		if blk.HostDispatch.State != hostDispatchStateStale || blk.HostDispatch.StatusPath != filepath.Join(rung(cwd), hostDispatchStatusFile) {
			t.Fatalf("state %q path %q; want stale against the first (cwd) hit", blk.HostDispatch.State, blk.HostDispatch.StatusPath)
		}
	})
}

// TestHostDispatchOverlay_StateDirTrust pins S4: a rung's directory must pass
// the dispatcher's own state-dir check. Each row plants a VALID PAUSED status
// in the rung under test, so only the trust check can turn it into absent.
func TestHostDispatchOverlay_StateDirTrust(t *testing.T) {
	paused := hdoStatus()
	paused.Paused = true
	runID, stageID := uuid.NewString(), uuid.NewString()
	otherUID := func(os.FileInfo) (int, bool) { return os.Getuid() + 1, true }
	run := func(t *testing.T, r *runResolver) *campaignItemAwaiting {
		t.Helper()
		b := newHDORuns(t)
		b.dirs[runID] = t.TempDir()
		if r.api == nil {
			r.api = newAPIClient(config{backendURL: b.srv.URL, apiToken: "tok-test"})
		}
		return hdoOverlayOne(t, r, runID, stageID)
	}
	expectSkipped := func(t *testing.T, blk *campaignItemAwaiting, want string) {
		t.Helper()
		if blk.Kind != "human" || blk.Reason != hostDispatchReasonAbsent || blk.HostDispatch.State != hostDispatchStateAbsent {
			t.Fatalf("awaiting = %s/%s state %q; want human/host_dispatcher_absent (next_step %q)", blk.Kind, blk.Reason, blk.HostDispatch.State, blk.NextStep)
		}
		hdoContainsAll(t, "next_step", blk.NextStep, want)
		hdoContainsAll(t, "paths_tried", strings.Join(blk.HostDispatch.PathsTried, "|"), want)
	}

	t.Run("a unowned", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd := t.TempDir()
		hdoWrite(t, filepath.Join(cwd, hostDispatchCheckoutStateDir), paused)
		r := hdoResolver(nil, cwd, nil)
		r.hostDispatchOwner = otherUID
		expectSkipped(t, run(t, r), filepath.Join(cwd, hostDispatchCheckoutStateDir)+" (skipped: not owned by this user)")
	})
	t.Run("b symlink", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd, real := t.TempDir(), filepath.Join(t.TempDir(), "real")
		hdoWrite(t, real, paused)
		link := filepath.Join(cwd, hostDispatchCheckoutStateDir)
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		expectSkipped(t, run(t, hdoResolver(nil, cwd, nil)), link+" (skipped: symlink)")
	})
	t.Run("c not a directory", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd := t.TempDir()
		path := filepath.Join(cwd, hostDispatchCheckoutStateDir)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		expectSkipped(t, run(t, hdoResolver(nil, cwd, nil)), path+" (skipped: not a directory)")
	})
	t.Run("d untrusted env rung stays exclusive", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		env, cwd := filepath.Join(t.TempDir(), "env"), t.TempDir()
		hdoWrite(t, env, paused)
		hdoWrite(t, filepath.Join(cwd, hostDispatchCheckoutStateDir), hdoStatus())
		envFI, err := os.Lstat(env)
		if err != nil {
			t.Fatal(err)
		}
		r := hdoResolver(map[string]string{hostDispatchStateDirEnv: env}, cwd, nil)
		r.hostDispatchOwner = func(fi os.FileInfo) (int, bool) {
			if os.SameFile(fi, envFI) {
				return os.Getuid() + 1, true
			}
			return fileOwnerUID(fi)
		}
		blk := run(t, r)
		expectSkipped(t, blk, env+" (skipped: not owned by this user)")
		if len(blk.HostDispatch.PathsTried) != 1 {
			t.Fatalf("paths_tried = %v; want only the env dir", blk.HostDispatch.PathsTried)
		}
	})
	t.Run("e control: the unowned fixture with the real owner reads paused", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd := t.TempDir()
		hdoWrite(t, filepath.Join(cwd, hostDispatchCheckoutStateDir), paused)
		blk := run(t, hdoResolver(nil, cwd, nil))
		if blk.Reason != hostDispatchReasonPaused {
			t.Fatalf("reason = %q; want host_dispatch_paused (the fixture must be valid)", blk.Reason)
		}
	})
}

// TestHostDispatchOverlay_BoundsStatusText pins the read-side bound: fixtures
// bypass the writer's bound, as a hostile or older writer would.
func TestHostDispatchOverlay_BoundsStatusText(t *testing.T) {
	runID, stageID := uuid.NewString(), uuid.NewString()
	// Control characters lead the fixture so they sit inside the 300-byte cut:
	// only the control-rune filter, not truncation, can remove them.
	long := "x\x1b[31m\n\u0085" + strings.Repeat("d", 2500) + "\x1b" + strings.Repeat("é", 1500)
	code400 := strings.Repeat("c", 400)
	bounded := func(t *testing.T, label, s string) {
		t.Helper()
		if len(s) == 0 || len(s) > hostDispatchTextMax || !utf8.ValidString(s) || hdHasControlRune(s) {
			t.Errorf("%s is %d bytes (utf8 %v, control rune %v); want 1..%d bytes, utf8, no control rune", label, len(s), utf8.ValidString(s), hdHasControlRune(s), hostDispatchTextMax)
		}
	}
	ref := func(code, stage, detail string) map[string]hostDispatchRunRefusal {
		return map[string]hostDispatchRunRefusal{runID: {RunID: runID, StageID: stage, Code: code, Detail: detail, FirstAt: hdoNow, LastAt: hdoNow}}
	}
	for _, tc := range []struct {
		name  string
		mut   func(*HostDispatcherStatus)
		state string
		check func(t *testing.T, blk *campaignItemAwaiting)
	}{
		{"a long detail with control characters", func(s *HostDispatcherStatus) { s.RunRefusals = ref(hdRefuseWorkingDirAbsent, stageID, long) }, hostDispatchStateRefusing,
			func(t *testing.T, blk *campaignItemAwaiting) {
				bounded(t, "refusal_detail", blk.HostDispatch.RefusalDetail)
				if hdHasControlRune(blk.NextStep) {
					t.Errorf("next_step carries a control rune: %q", blk.NextStep)
				}
			}},
		{"b long unknown code", func(s *HostDispatcherStatus) { s.RunRefusals = ref(code400, stageID, "") }, hostDispatchStateRefusing,
			func(t *testing.T, blk *campaignItemAwaiting) {
				bounded(t, "refusal_code", blk.HostDispatch.RefusalCode)
			}},
		{"c long tick failure code", func(s *HostDispatcherStatus) {
			s.LastTickFailure = &hostDispatchTickFailure{Code: code400, At: s.LastTickAt}
		}, hostDispatchStateRefusing,
			func(t *testing.T, blk *campaignItemAwaiting) {
				bounded(t, "tick_failure_code", blk.HostDispatch.TickFailureCode)
			}},
		{"d long mismatched stage id", func(s *HostDispatcherStatus) { s.RunRefusals = ref(hdRefuseWorkingDirAbsent, code400, "") }, hostDispatchStateUnverified,
			func(t *testing.T, blk *campaignItemAwaiting) {
				bounded(t, "refusal_stage_id", blk.HostDispatch.RefusalStageID)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdoEmptyTMPDIR(t)
			st := hdoStatus()
			tc.mut(&st)
			dir := filepath.Join(t.TempDir(), "state")
			hdoWrite(t, dir, st)
			blk := hdoOverlayOne(t, hdoResolver(map[string]string{hostDispatchStateDirEnv: dir}, t.TempDir(), nil), runID, stageID)
			if blk.HostDispatch.State != tc.state {
				t.Fatalf("state = %q; want %q", blk.HostDispatch.State, tc.state)
			}
			tc.check(t, blk)
		})
	}
}

// TestOverlayCampaignHostDispatch_ReadCapPausedAppliesToEveryItem pins R1: a
// status on a global rung (here TMPDIR) applies to EVERY item, including those
// the read cap skipped.
func TestOverlayCampaignHostDispatch_ReadCapPausedAppliesToEveryItem(t *testing.T) {
	build := func(t *testing.T) ([]CampaignItem, *hdoRuns) {
		b := newHDORuns(t)
		items := make([]CampaignItem, 17)
		for i := range items {
			id := uuid.NewString()
			b.dirs[id] = t.TempDir()
			items[i] = hdoItem(id, uuid.NewString())
		}
		return items, b
	}
	t.Run("paused on TMPDIR reaches item 17", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		paused := hdoStatus()
		paused.Paused = true
		hdoWrite(t, tmpRung, paused)
		items, b := build(t)
		hdoResolver(nil, t.TempDir(), b).overlayCampaignHostDispatch(context.Background(), items)
		for i, it := range items {
			if it.Awaiting.Kind != "human" || it.Awaiting.Reason != hostDispatchReasonPaused {
				t.Errorf("item %d = %s/%s; want human/host_dispatch_paused", i+1, it.Awaiting.Kind, it.Awaiting.Reason)
			}
		}
		if b.count() != hostDispatchOverlayReadCap {
			t.Fatalf("run reads = %d; want exactly %d", b.count(), hostDispatchOverlayReadCap)
		}
	})
	t.Run("no status anywhere: item 17 unverified", func(t *testing.T) {
		tmpRung := hdoEmptyTMPDIR(t)
		items, b := build(t)
		cwd := t.TempDir()
		hdoResolver(nil, cwd, b).overlayCampaignHostDispatch(context.Background(), items)
		for i, it := range items[:16] {
			if it.Awaiting.Reason != hostDispatchReasonAbsent {
				t.Errorf("item %d reason = %q; want host_dispatcher_absent", i+1, it.Awaiting.Reason)
			}
		}
		last := items[16].Awaiting
		if last.Kind != hostDispatchAwaitKind || last.Reason != hostDispatchReasonUnverified {
			t.Fatalf("item 17 = %s/%s; want host_dispatcher/host_dispatch_unverified", last.Kind, last.Reason)
		}
		hdoContainsAll(t, "item 17 next_step", last.NextStep, "fishhawk-mcp host-dispatch --status", "read cap")
		// Each item's ladder names only its own run's working_dir rung.
		for i, it := range items {
			want := []string{filepath.Join(cwd, hostDispatchCheckoutStateDir)}
			if i < hostDispatchOverlayReadCap {
				want = append(want, filepath.Join(b.dirs[it.Awaiting.RunID], hostDispatchCheckoutStateDir))
			}
			want = append(want, tmpRung)
			if got := it.Awaiting.HostDispatch.PathsTried; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("item %d paths_tried = %v; want %v", i+1, got, want)
			}
		}
	})
}

// TestHostDispatchBoundedPaths pins the paths_tried bound: at most
// hostDispatchPathsTriedMax entries, the last a "(+N more)".
func TestHostDispatchBoundedPaths(t *testing.T) {
	var in []string
	for i := range 9 {
		in = append(in, fmt.Sprintf("/d%d", i))
	}
	got := hostDispatchBoundedPaths(in)
	if len(got) != hostDispatchPathsTriedMax || got[len(got)-1] != "(+4 more)" || got[0] != "/d0" {
		t.Fatalf("bounded = %v; want %d entries, /d0 first, ending (+4 more)", got, hostDispatchPathsTriedMax)
	}
	if got := hostDispatchBoundedPaths(in[:3]); fmt.Sprint(got) != fmt.Sprint(in[:3]) {
		t.Fatalf("bounded short list = %v; want it unchanged", got)
	}
}

// TestOverlayCampaignHostDispatch_WorkingDirRungSpeaksOnlyForItsRun pins the
// 27e34411 decision: a status found only on run A's working_dir rung decides
// A's items and never another run's. Run B's item continues down its own
// ladder (its own rung, then TMPDIR) and never reads A's live status. Both
// item orders run, so neither ordering can let A's hit leak into B.
func TestOverlayCampaignHostDispatch_WorkingDirRungSpeaksOnlyForItsRun(t *testing.T) {
	runA, runB := uuid.NewString(), uuid.NewString()
	rung := func(root string) string { return filepath.Join(root, hostDispatchCheckoutStateDir) }
	for _, tc := range []struct {
		name   string
		tmp    *HostDispatcherStatus // the real dispatcher on TMPDIR; nil is absent
		bState string
		bWhy   string
	}{
		{name: "real state absent", bState: hostDispatchStateAbsent, bWhy: hostDispatchReasonAbsent},
		{name: "real state paused", tmp: func() *HostDispatcherStatus {
			st := hdoStatus()
			st.Paused = true
			return &st
		}(), bState: hostDispatchStatePaused, bWhy: hostDispatchReasonPaused},
	} {
		for _, order := range []string{"A first", "B first"} {
			t.Run(tc.name+", "+order, func(t *testing.T) {
				tmpRung := hdoEmptyTMPDIR(t)
				if tc.tmp != nil {
					hdoWrite(t, tmpRung, *tc.tmp)
				}
				cwd, wdA, wdB := t.TempDir(), t.TempDir(), t.TempDir()
				hdoWrite(t, rung(wdA), hdoStatus()) // planted live status in A's checkout
				b := newHDORuns(t)
				b.dirs[runA], b.dirs[runB] = wdA, wdB
				itemA, itemB := hdoItem(runA, uuid.NewString()), hdoItem(runB, uuid.NewString())
				items := []CampaignItem{itemA, itemB}
				if order == "B first" {
					items = []CampaignItem{itemB, itemA}
				}
				hdoResolver(nil, cwd, b).overlayCampaignHostDispatch(context.Background(), items)
				var ga, gb *campaignItemAwaiting
				for _, it := range items {
					if it.Awaiting.RunID == runA {
						ga = it.Awaiting
					} else {
						gb = it.Awaiting
					}
				}
				if gb.HostDispatch.State != tc.bState || gb.Reason != tc.bWhy {
					t.Fatalf("B = %s/%s state %q (status_path %q); want state %q reason %s, never A's planted live status",
						gb.Kind, gb.Reason, gb.HostDispatch.State, gb.HostDispatch.StatusPath, tc.bState, tc.bWhy)
				}
				if gb.NextStep == hdoBackendNext {
					t.Errorf("B next_step is the backend's live text")
				}
				hdoContainsNone(t, "B paths_tried", strings.Join(gb.HostDispatch.PathsTried, "|"), rung(wdA))
				if ga.HostDispatch.State != hostDispatchStateLive || ga.HostDispatch.StatusPath != filepath.Join(rung(wdA), hostDispatchStatusFile) {
					t.Fatalf("A state %q path %q; want live from its own working_dir rung", ga.HostDispatch.State, ga.HostDispatch.StatusPath)
				}
			})
		}
	}
}

// TestOverlayCampaignHostDispatch_GetRunErrorIsUnverified pins R1: a run whose
// read failed is unverified, never absent, when no status was found.
func TestOverlayCampaignHostDispatch_GetRunErrorIsUnverified(t *testing.T) {
	a, bID := uuid.NewString(), uuid.NewString()
	t.Run("no status anywhere", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		b := newHDORuns(t)
		b.fail[a] = true
		b.dirs[bID] = t.TempDir()
		items := []CampaignItem{hdoItem(a, uuid.NewString()), hdoItem(bID, uuid.NewString())}
		hdoResolver(nil, t.TempDir(), b).overlayCampaignHostDispatch(context.Background(), items)
		ga, gb := items[0].Awaiting, items[1].Awaiting
		if ga.Kind != hostDispatchAwaitKind || ga.Reason != hostDispatchReasonUnverified {
			t.Fatalf("A = %s/%s; want host_dispatcher/host_dispatch_unverified", ga.Kind, ga.Reason)
		}
		hdoContainsAll(t, "A next_step", ga.NextStep, "fishhawk-mcp host-dispatch --status")
		hdoContainsNone(t, "A next_step", ga.NextStep, "no operator action is needed")
		if gb.Kind != "human" || gb.Reason != hostDispatchReasonAbsent {
			t.Fatalf("B = %s/%s; want human/host_dispatcher_absent", gb.Kind, gb.Reason)
		}
	})
	t.Run("a paused cwd status needs no run read", func(t *testing.T) {
		hdoEmptyTMPDIR(t)
		cwd := t.TempDir()
		paused := hdoStatus()
		paused.Paused = true
		hdoWrite(t, filepath.Join(cwd, hostDispatchCheckoutStateDir), paused)
		b := newHDORuns(t)
		b.fail[a] = true
		items := []CampaignItem{hdoItem(a, uuid.NewString())}
		hdoResolver(nil, cwd, b).overlayCampaignHostDispatch(context.Background(), items)
		if items[0].Awaiting.Reason != hostDispatchReasonPaused || b.count() != 0 {
			t.Fatalf("A reason %q, reads %d; want host_dispatch_paused with zero reads", items[0].Awaiting.Reason, b.count())
		}
	})
}

// hdoCallOverSession calls one registered tool from a real client over an
// in-memory transport, so the SDK's output-schema validation runs on the
// marshalled host_dispatch blocks, and returns the structured JSON.
func hdoCallOverSession(t *testing.T, r *runResolver, name string, args map[string]any) string {
	t.Helper()
	ctx := context.Background()
	srv := buildServer(config{backendURL: "http://unused", apiToken: "tok"})
	registerTools(srv, r)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		raw, _ := json.Marshal(res.Content)
		t.Fatalf("%s returned a tool error (output-schema mismatch?): %s", name, raw)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// hdoWireState decodes the wire JSON and returns host_dispatch.state under the
// object at path (map keys; an int indexes an array).
func hdoWireState(t *testing.T, wire string, path ...any) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(wire), &v); err != nil {
		t.Fatalf("decode wire output: %v", err)
	}
	for _, p := range append(path, "host_dispatch", "state") {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return ""
			}
			v = a[k]
		}
	}
	s, _ := v.(string)
	return s
}
