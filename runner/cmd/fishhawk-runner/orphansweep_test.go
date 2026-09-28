package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/procsweep"
)

// enableOrphanSweepForTest opts one test into the REAL orphan sweeper, which
// runTestMain disables package-wide (#3663). Restored on cleanup.
func enableOrphanSweepForTest(t *testing.T) {
	t.Helper()
	prev := orphanSweepDefaultOn
	orphanSweepDefaultOn = true
	t.Cleanup(func() { orphanSweepDefaultOn = prev })
}

// withSelfProcessGroup poses the runner's process group, so the group-leader
// guard can be armed or disarmed deterministically.
func withSelfProcessGroup(t *testing.T, pgid func(int) int) {
	t.Helper()
	prev := selfProcessGroup
	selfProcessGroup = pgid
	t.Cleanup(func() { selfProcessGroup = prev })
}

// killRecorder is a concurrency-safe record of the pids a sweep asked to kill.
// Guarded because the sweeper's sampler goroutine and the stage goroutine both
// reach the procsweep seams (#3226: a fake reachable from two goroutines guards
// its own bookkeeping).
type killRecorder struct {
	mu   sync.Mutex
	pids []int
	err  error
}

func (k *killRecorder) kill(pid int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pids = append(k.pids, pid)
	return k.err
}

func (k *killRecorder) seen() []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]int(nil), k.pids...)
}

// syntheticTable builds a process table in which pid `self` is the runner and
// `orphan` is a descendant of it — the shape the sweeper must reap.
func syntheticTable(self, orphan int, start time.Time) procsweep.Table {
	mk := func(pid, ppid, pgid int, cmd string) procsweep.Proc {
		return procsweep.Proc{
			PID: pid, PPID: ppid, PGID: pgid,
			Start:    start,
			StartRaw: procsweep.FormatStart(start),
			Command:  cmd,
		}
	}
	return procsweep.Table{Procs: []procsweep.Proc{
		mk(self, 1, self, "fishhawk-runner"),
		mk(orphan, self, self, "sh -c while :; do :; done"),
	}}
}

// sweeperPayload decodes the stage_orphans_reaped payload.
type sweeperPayload struct {
	Count       int `json:"count"`
	AlreadyGone int `json:"already_gone"`
	Sampled     []struct {
		PID     int    `json:"pid"`
		Command string `json:"command"`
	} `json:"sampled"`
	Errors []string `json:"errors"`
}

func decodeSweeperEvent(t *testing.T, ev *agent.Event) sweeperPayload {
	t.Helper()
	if ev == nil {
		t.Fatal("sweep returned nil, want a stage_orphans_reaped event")
	}
	if ev.Kind != "stage_orphans_reaped" {
		t.Fatalf("event kind = %q, want stage_orphans_reaped", ev.Kind)
	}
	var p sweeperPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload decode: %v\n%s", err, ev.Payload)
	}
	return p
}

// wantDegrade asserts the sweep took the named fail-open degrade: no bundle
// event (a degrade is LOG-ONLY, so an existing stage's trace never grows one),
// the reason recorded and PRINTED, and zero kills.
func wantDegrade(t *testing.T, s *stageOrphanSweeper, ev *agent.Event, log string, reason string, kr *killRecorder) {
	t.Helper()
	if ev != nil {
		t.Fatalf("a degrade must return NO bundle event, got %s", ev.Payload)
	}
	if got := s.degradeReason(); got != reason {
		t.Fatalf("degrade reason = %q, want %q\nlog: %s", got, reason, log)
	}
	if !strings.Contains(log, `"event":"stage_orphan_sweep_degraded"`) || !strings.Contains(log, reason) {
		t.Fatalf("the degrade must print one named reason, got:\n%s", log)
	}
	if kr != nil {
		if got := kr.seen(); len(got) != 0 {
			t.Fatalf("killPID calls = %v, want none under degrade %q", got, reason)
		}
	}
}

func sweepCfg() config {
	return config{runID: "11111111-2222-3333-4444-555555555555", stageID: "66666666-7777-8888-9999-000000000000"}
}

// noEnv is the getenv seam for a test that sets no operator escape.
func noEnv(string) string { return "" }

// --- the happy path -----------------------------------------------------

func TestStageOrphanSweeper_ReapsRecordedDescendantAndEmitsEvent(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid }) // runner leads its own group

	self := os.Getpid()
	start := time.Now()
	tbl := syntheticTable(self, 424242, start)
	kr := &killRecorder{}
	restore := procsweep.SetTestHooks(
		func(context.Context) (procsweep.Table, error) { return tbl, nil },
		kr.kill)
	t.Cleanup(restore)

	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), noEnv, start.Add(-time.Minute))
	s.start(context.Background())
	// Sample synchronously so the assertion never races the ticker.
	if err := s.rec.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	ev := s.sweep(context.Background())

	p := decodeSweeperEvent(t, ev)
	if p.Count != 1 {
		t.Fatalf("count = %d, want 1\npayload: %s\nlog: %s", p.Count, ev.Payload, log.String())
	}
	if r := s.degradeReason(); r != "" {
		t.Fatalf("degrade reason = %q, want empty on the reap path", r)
	}
	if len(p.Sampled) != 1 || p.Sampled[0].PID != 424242 {
		t.Fatalf("sampled = %+v, want the reaped pid 424242", p.Sampled)
	}
	if got := kr.seen(); len(got) != 1 || got[0] != 424242 {
		t.Fatalf("killPID calls = %v, want [424242]", got)
	}
	if !strings.Contains(log.String(), `"event":"stage_orphans_reaped"`) {
		t.Errorf("no stage_orphans_reaped log line:\n%s", log.String())
	}
}

// The zero-orphan common case emits NOTHING, so a clean stage's trace is
// byte-identical to before this change.
func TestStageOrphanSweeper_ZeroOrphansEmitsNoEvent(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })

	self := os.Getpid()
	start := time.Now()
	only := procsweep.Table{Procs: []procsweep.Proc{{
		PID: self, PPID: 1, PGID: self, Start: start,
		StartRaw: procsweep.FormatStart(start), Command: "fishhawk-runner",
	}}}
	kr := &killRecorder{}
	t.Cleanup(procsweep.SetTestHooks(func(context.Context) (procsweep.Table, error) { return only, nil }, kr.kill))

	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), noEnv, start.Add(-time.Minute))
	if err := s.rec.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if ev := s.sweep(context.Background()); ev != nil {
		t.Fatalf("sweep returned an event with no orphans: %s", ev.Payload)
	}
	if got := kr.seen(); len(got) != 0 {
		t.Fatalf("killPID calls = %v, want none", got)
	}
	if log.String() != "" {
		t.Errorf("the zero-orphan case must log nothing, got:\n%s", log.String())
	}
	if r := s.degradeReason(); r != "" {
		t.Errorf("degrade reason = %q — the nil event must come from the zero-orphan path, not a degrade", r)
	}
}

// sweep is sync.Once-guarded: the post-invoke call and the run() defer backstop
// cannot both perform a kill pass.
func TestStageOrphanSweeper_SweepIsOnceGuarded(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })

	self := os.Getpid()
	start := time.Now()
	kr := &killRecorder{}
	t.Cleanup(procsweep.SetTestHooks(
		func(context.Context) (procsweep.Table, error) { return syntheticTable(self, 424243, start), nil },
		kr.kill))

	s := newStageOrphanSweeper(&strings.Builder{}, sweepCfg(), noEnv, start.Add(-time.Minute))
	if err := s.rec.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	first := s.sweep(context.Background())
	second := s.sweep(context.Background())
	if first == nil {
		t.Fatal("first sweep returned no event")
	}
	if second != first {
		t.Fatal("the second sweep must return the SAME event, not perform another kill pass")
	}
	if got := kr.seen(); len(got) != 1 {
		t.Fatalf("killPID calls = %v, want exactly 1 across two sweep() calls", got)
	}
}

// --- the named degrades, one behavioral test each -----------------------

// i2 / m12 (approval condition 4): with no opt-in, the package test binary's
// kill switch makes the sweeper a no-op with a named reason.
func TestOrphanSweeperDisabledByDefaultUnderTestBinary(t *testing.T) {
	if orphanSweepDefaultOn {
		t.Fatal("runTestMain must default orphanSweepDefaultOn to false for this package's test binary")
	}
	kr := &killRecorder{}
	t.Cleanup(procsweep.SetTestHooks(
		func(context.Context) (procsweep.Table, error) {
			return syntheticTable(os.Getpid(), 424244, time.Now()), nil
		}, kr.kill))

	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), noEnv, time.Now().Add(-time.Minute))
	s.start(context.Background()) // must not sample either
	wantDegrade(t, s, s.sweep(context.Background()), log.String(), "disabled_in_test_binary", kr)
}

// m1: FISHHAWK_ORPHAN_SWEEP=off — the operator escape on a shared host.
func TestStageOrphanSweeper_OperatorOffSwitch(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })
	kr := &killRecorder{}
	t.Cleanup(procsweep.SetTestHooks(
		func(context.Context) (procsweep.Table, error) {
			return syntheticTable(os.Getpid(), 424245, time.Now()), nil
		}, kr.kill))

	for _, val := range []string{"off", "OFF", " Off "} {
		t.Run(val, func(t *testing.T) {
			var log strings.Builder
			s := newStageOrphanSweeper(&log, sweepCfg(), func(string) string { return val }, time.Now().Add(-time.Minute))
			wantDegrade(t, s, s.sweep(context.Background()), log.String(), "disabled_by_operator", kr)
		})
	}
	// Control: any OTHER value leaves the sweep armed, so the off-switch is a
	// specific opt-out and not "any value disables".
	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), func(string) string { return "on" }, time.Now().Add(-time.Minute))
	if s.disabled != "" {
		t.Fatalf("FISHHAWK_ORPHAN_SWEEP=on disabled the sweep (%q)", s.disabled)
	}
}

// m2: the runner is not its own process-group leader and no descendant pgids
// were recorded — nothing this sweep could ever authorise, so it declines before
// reading the table at all.
func TestStageOrphanSweeper_NotGroupLeaderWithNothingRecorded(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(int) int { return 4242 }) // somebody else's group
	kr := &killRecorder{}
	reads := 0
	t.Cleanup(procsweep.SetTestHooks(func(context.Context) (procsweep.Table, error) {
		reads++
		return syntheticTable(os.Getpid(), 424246, time.Now()), nil
	}, kr.kill))

	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), noEnv, time.Now().Add(-time.Minute))
	wantDegrade(t, s, s.sweep(context.Background()), log.String(), "not_group_leader_no_recorded_pgids", kr)
	if reads != 0 {
		t.Errorf("the table was read %d time(s); this degrade must decline before reading", reads)
	}
}

// m3: a ps read error at sweep time — fail open with a named reason, zero kills.
func TestStageOrphanSweeper_ProcTableReadErrorFailsOpen(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })
	kr := &killRecorder{}
	t.Cleanup(procsweep.SetTestHooks(nil, kr.kill))

	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), noEnv, time.Now().Add(-time.Minute))
	s.sweepFn = func(context.Context, int, int) (procsweep.Result, error) {
		return procsweep.Result{}, errors.New("ps: exec format error")
	}
	wantDegrade(t, s, s.sweep(context.Background()), log.String(), "proc_table_read_failed", kr)
	if !strings.Contains(log.String(), "exec format error") {
		t.Errorf("the degrade must print the underlying read error:\n%s", log.String())
	}
}

// m4: procsweep.ErrUnsupported (the Windows build's seams) is reported as its own
// named reason, distinct from an ordinary read failure.
func TestStageOrphanSweeper_UnsupportedPlatformDegrade(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })
	kr := &killRecorder{}
	t.Cleanup(procsweep.SetTestHooks(nil, kr.kill))

	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), noEnv, time.Now().Add(-time.Minute))
	s.sweepFn = func(context.Context, int, int) (procsweep.Result, error) {
		return procsweep.Result{}, procsweep.ErrUnsupported
	}
	wantDegrade(t, s, s.sweep(context.Background()), log.String(), "proc_table_unsupported", kr)
}

// m7 at the wiring layer: a kill error is reported as evidence, never as a stage
// failure — sweep() returns an event, not an error, and the caller appends it.
func TestStageOrphanSweeper_KillErrorsAreReportedNotFatal(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })
	t.Cleanup(procsweep.SetTestHooks(nil, nil))

	var log strings.Builder
	s := newStageOrphanSweeper(&log, sweepCfg(), noEnv, time.Now().Add(-time.Minute))
	s.sweepFn = func(context.Context, int, int) (procsweep.Result, error) {
		return procsweep.Result{AlreadyGone: 2, Errors: []string{"kill 5 (x): operation not permitted"}}, nil
	}
	p := decodeSweeperEvent(t, s.sweep(context.Background()))
	if p.Count != 0 || p.AlreadyGone != 2 {
		t.Fatalf("count/already_gone = %d/%d, want 0/2", p.Count, p.AlreadyGone)
	}
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0], "kill 5") {
		t.Fatalf("errors = %v, want the collected kill failure", p.Errors)
	}
}

// The event's sampled list is bounded so a pathological stage cannot balloon the
// bundle.
func TestStageOrphanSweeper_SampledListIsBounded(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })
	t.Cleanup(procsweep.SetTestHooks(nil, nil))

	reaped := make([]procsweep.Proc, 0, orphanSweepMaxSampled+7)
	for i := 0; i < orphanSweepMaxSampled+7; i++ {
		reaped = append(reaped, procsweep.Proc{PID: 9000 + i, Command: "busyloop"})
	}
	s := newStageOrphanSweeper(&strings.Builder{}, sweepCfg(), noEnv, time.Now().Add(-time.Minute))
	s.sweepFn = func(context.Context, int, int) (procsweep.Result, error) {
		return procsweep.Result{Reaped: reaped}, nil
	}
	p := decodeSweeperEvent(t, s.sweep(context.Background()))
	if p.Count != len(reaped) {
		t.Errorf("count = %d, want the full %d", p.Count, len(reaped))
	}
	if len(p.Sampled) != orphanSweepMaxSampled {
		t.Fatalf("sampled = %d entries, want the %d-entry bound", len(p.Sampled), orphanSweepMaxSampled)
	}
}

// --- CROSS-BOUNDARY -----------------------------------------------------

// TestRunStage_SweepsOrphansAndEmitsEvent drives run()'s real stage path with a
// synthetic process table and a fake invoker whose Invoke registers a synthetic
// descendant, asserting BOTH that the recorded pid was killed AND that the
// stage_orphans_reaped event reached the SIGNED BUNDLE — the seam between the
// procsweep library and the runner's bundle assembly (composeGateEvidence /
// bundle.PackBytes) that neither package's units can see on their own.
func TestRunStage_SweepsOrphansAndEmitsEvent(t *testing.T) {
	enableOrphanSweepForTest(t)
	withSelfProcessGroup(t, func(pid int) int { return pid })

	const orphanPID = 424247
	self := os.Getpid()
	start := time.Now()

	// The table is EMPTY until the agent runs, so the descendant only becomes
	// visible because Invoke registered it — proving the recorder samples across
	// the stage rather than once at construction.
	var mu sync.Mutex
	registered := false
	kr := &killRecorder{}
	t.Cleanup(procsweep.SetTestHooks(func(context.Context) (procsweep.Table, error) {
		mu.Lock()
		on := registered
		mu.Unlock()
		if !on {
			return procsweep.Table{Procs: []procsweep.Proc{{
				PID: self, PPID: 1, PGID: self, Start: start,
				StartRaw: procsweep.FormatStart(start), Command: "fishhawk-runner",
			}}}, nil
		}
		return syntheticTable(self, orphanPID, start), nil
	}, kr.kill))

	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(int, agent.Invocation) {
			mu.Lock()
			registered = true
			mu.Unlock()
		},
	})

	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(dir, "bundle.json.gz")

	var stderr strings.Builder
	if got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", promptPath,
		"--bundle-out", bundlePath,
	}, &stderr); got != exitOK {
		t.Fatalf("run = %d, want %d:\n%s", got, exitOK, stderr.String())
	}

	if got := kr.seen(); len(got) != 1 || got[0] != orphanPID {
		t.Fatalf("killPID calls = %v, want [%d] — the recorded descendant was not reaped by run()", got, orphanPID)
	}
	if !strings.Contains(stderr.String(), `"event":"stage_orphans_reaped"`) {
		t.Errorf("run() logged no stage_orphans_reaped line:\n%s", stderr.String())
	}

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	text, err := gunzip(data)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !strings.Contains(string(text), "stage_orphans_reaped") {
		t.Fatalf("the stage_orphans_reaped event did not reach the bundle:\n%s", text)
	}
	if !strings.Contains(string(text), `"command":"sh -c while :; do :; done"`) {
		t.Errorf("the bundled event does not carry the reaped process's command:\n%s", text)
	}
}
