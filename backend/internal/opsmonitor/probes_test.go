package opsmonitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/hosthealth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stalesweep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

func one(t *testing.T, rs []Result) Result {
	t.Helper()
	if len(rs) != 1 {
		t.Fatalf("results = %+v, want exactly one", rs)
	}
	return rs[0]
}

// --- disk -------------------------------------------------------------------

func TestDiskProbe(t *testing.T) {
	statfs := func(avail uint64, err error) func(string) (uint64, uint64, error) {
		return func(string) (uint64, uint64, error) { return avail, 100 * gib, err }
	}
	for name, tc := range map[string]struct {
		probe DiskProbe
		want  Status
		cond  string
	}{
		"statfs error": {DiskProbe{Path: "/data", MinFreeBytes: gib, Statfs: statfs(0, errors.New("eio"))}, StatusUnknown, ""},
		"empty path":   {DiskProbe{MinFreeBytes: gib, Statfs: statfs(50*gib, nil)}, StatusUnknown, ""},
		"below min":    {DiskProbe{Path: "/data", MinFreeBytes: 20 * gib, Statfs: statfs(5*gib, nil)}, StatusFailing, ConditionDiskFreeLow},
		"above min":    {DiskProbe{Path: "/data", MinFreeBytes: 20 * gib, Statfs: statfs(50*gib, nil)}, StatusOK, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, tc.probe.Check(context.Background()))
			if r.Status != tc.want || r.Condition != tc.cond {
				t.Errorf("result = %+v, want %s %s", r, tc.want, tc.cond)
			}
			if tc.want == StatusFailing && (r.Observed != "5.0 GiB free of 100.0 GiB" || r.Threshold != ">= 20.0 GiB free" || r.Detail["path"] != "/data") {
				t.Errorf("failing result = %+v", r)
			}
		})
	}
	if !(DiskProbe{}).HostLocal() {
		t.Error("disk must be host-local")
	}
}

// TestDiskProbe_RealStatfs pins the real syscall arithmetic.
func TestDiskProbe_RealStatfs(t *testing.T) {
	r := one(t, DiskProbe{Path: t.TempDir(), MinFreeBytes: 1 << 60}.Check(context.Background()))
	if r.Status != StatusFailing || r.Condition != ConditionDiskFreeLow {
		t.Fatalf("result = %+v, want failing against a 1 EiB minimum", r)
	}
	avail, _ := r.Detail["avail_bytes"].(uint64)
	total, _ := r.Detail["total_bytes"].(uint64)
	if avail == 0 || total < avail {
		t.Errorf("avail %d, total %d; want 0 < avail <= total", avail, total)
	}
}

// --- gocache ----------------------------------------------------------------

func goCacheFixture(t *testing.T, marker bool, files map[string]int) string {
	t.Helper()
	dir := t.TempDir()
	readme := "unrelated"
	if marker {
		readme = "This directory holds cached build artifacts from the Go build system.\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte(readme), 0o600); err != nil {
		t.Fatal(err)
	}
	for rel, size := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestGoCacheProbe_ResolveFailuresAreUnknown(t *testing.T) {
	for name, resolve := range map[string]func(context.Context) (string, error){
		"error": func(context.Context) (string, error) { return "", errors.New("go: not found") },
		"empty": func(context.Context) (string, error) { return "", nil },
		"off":   func(context.Context) (string, error) { return "off", nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, GoCacheProbe{MaxBytes: 1, ResolveDir: resolve}.Check(context.Background()))
			if r.Status != StatusUnknown {
				t.Errorf("result = %+v, want unknown", r)
			}
		})
	}
}

func TestGoCacheProbe_MissingRootIsUnknown(t *testing.T) {
	r := one(t, GoCacheProbe{Dir: filepath.Join(t.TempDir(), "absent"), MaxBytes: 1}.Check(context.Background()))
	if r.Status != StatusUnknown || !strings.Contains(r.Reason, "missing") {
		t.Errorf("result = %+v, want unknown missing", r)
	}
}

func TestGoCacheProbe_NoMarkerIsUnknown(t *testing.T) {
	dir := goCacheFixture(t, false, nil)
	r := one(t, GoCacheProbe{Dir: dir, MaxBytes: 1}.Check(context.Background()))
	if r.Status != StatusUnknown || !strings.Contains(r.Reason, "not a Go build cache") {
		t.Errorf("result = %+v, want unknown 'not a Go build cache'", r)
	}
}

func TestGoCacheProbe_SampledEstimate(t *testing.T) {
	files := map[string]int{
		"00/aa-d":     1000,
		"10/bb-a":     24,
		"01/unsample": 1 << 20, // not one of the 16 sampled shards
	}
	dir := goCacheFixture(t, true, files)
	if err := os.MkdirAll(filepath.Join(dir, "20", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	const want = (1000 + 24) * 16
	resolved := false
	over := one(t, GoCacheProbe{MaxBytes: want - 1, ResolveDir: func(context.Context) (string, error) {
		resolved = true
		return dir, nil
	}}.Check(context.Background()))
	if !resolved {
		t.Error("empty Dir did not resolve lazily")
	}
	if over.Status != StatusFailing || over.Condition != ConditionGoCacheOverThreshold || over.Detail["estimate_bytes"] != uint64(want) {
		t.Errorf("over = %+v, want failing with estimate %d", over, want)
	}
	under := one(t, GoCacheProbe{Dir: dir, MaxBytes: want, ResolveDir: func(context.Context) (string, error) {
		t.Error("ResolveDir called although Dir is set")
		return "", nil
	}}.Check(context.Background()))
	if under.Status != StatusOK {
		t.Errorf("under = %+v, want ok", under)
	}
}

func TestGoCacheProbe_CancelledWalkIsUnknown(t *testing.T) {
	dir := goCacheFixture(t, true, map[string]int{"00/a": 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := one(t, GoCacheProbe{Dir: dir, MaxBytes: 1 << 40}.Check(ctx))
	if r.Status != StatusUnknown || !strings.Contains(r.Reason, "cancelled") {
		t.Errorf("result = %+v, want unknown cancelled", r)
	}
}

func TestResolveGoCacheDirWith(t *testing.T) {
	got, err := resolveGoCacheDirWith(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "go" || strings.Join(args, " ") != "env GOCACHE" {
			t.Errorf("exec = %s %v", name, args)
		}
		return []byte("/home/u/.cache/go-build\n"), nil
	})
	if err != nil || got != "/home/u/.cache/go-build" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := resolveGoCacheDirWith(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("not found")
	}); err == nil || !strings.Contains(err.Error(), "go env GOCACHE") {
		t.Errorf("err = %v, want wrapped", err)
	}
}

// --- load -------------------------------------------------------------------

func TestLoadProbe(t *testing.T) {
	read := func(v float64, err error) func(context.Context) (float64, error) {
		return func(context.Context) (float64, error) { return v, err }
	}
	cores := func(n int) func() int { return func() int { return n } }
	for name, tc := range map[string]struct {
		probe LoadProbe
		want  Status
	}{
		"read error":     {LoadProbe{Factor: 4, ReadLoad: read(0, errors.New("no source")), Cores: cores(10)}, StatusUnknown},
		"zero cores":     {LoadProbe{Factor: 4, ReadLoad: read(1, nil), Cores: cores(0)}, StatusUnknown},
		"above limit":    {LoadProbe{Factor: 4, ReadLoad: read(41, nil), Cores: cores(10)}, StatusFailing},
		"below limit":    {LoadProbe{Factor: 4, ReadLoad: read(39, nil), Cores: cores(10)}, StatusOK},
		"default factor": {LoadProbe{ReadLoad: read(41, nil), Cores: cores(10)}, StatusFailing},
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, tc.probe.Check(context.Background()))
			if r.Status != tc.want {
				t.Errorf("result = %+v, want %s", r, tc.want)
			}
			if tc.want == StatusFailing && (r.Condition != ConditionHostLoadHigh || r.Threshold != "<= 40.0 (4.0 x 10 cores)") {
				t.Errorf("failing result = %+v", r)
			}
		})
	}
}

// TestLoadProbe_Attribution (E85.11): an over-limit load is classified by
// what it is made of. Load 160 on 14 cores (limit 56) with 150
// uninterruptible vs 3 runnable is swap/I-O thrash -> host_load_io_wait.
func TestLoadProbe_Attribution(t *testing.T) {
	read := func(v float64) func(context.Context) (float64, error) {
		return func(context.Context) (float64, error) { return v, nil }
	}
	attr := func(a hosthealth.LoadAttribution, err error) func(context.Context) (hosthealth.LoadAttribution, error) {
		return func(context.Context) (hosthealth.LoadAttribution, error) { return a, err }
	}
	for name, tc := range map[string]struct {
		load          float64
		attribution   func(context.Context) (hosthealth.LoadAttribution, error)
		wantStatus    Status
		wantCondition string
		wantObserved  string
	}{
		"wait dominated": {160, attr(hosthealth.LoadAttribution{Load1: 160, Runnable: 3, Uninterruptible: 150}, nil),
			StatusFailing, ConditionHostLoadIOWait, "1-minute load 160.00 on 14 cores; 3 runnable vs 150 uninterruptible"},
		"psi io dominated": {160, attr(hosthealth.LoadAttribution{Runnable: 90, Uninterruptible: 2, HasPSI: true, IOSomeAvg10: 70, CPUSomeAvg10: 10}, nil),
			StatusFailing, ConditionHostLoadIOWait, "1-minute load 160.00 on 14 cores; 90 runnable vs 2 uninterruptible; PSI io some avg10 70.00 vs cpu some avg10 10.00"},
		"runnable dominated": {160, attr(hosthealth.LoadAttribution{Runnable: 150, Uninterruptible: 3}, nil),
			StatusFailing, ConditionHostLoadHigh, "1-minute load 160.00 on 14 cores; 150 runnable vs 3 uninterruptible"},
		"attribution error": {160, attr(hosthealth.LoadAttribution{Runnable: 3, Uninterruptible: 150}, errors.New("ps failed")),
			StatusFailing, ConditionHostLoadHigh, "1-minute load 160.00 on 14 cores"},
		"under limit never attributes": {20, func(context.Context) (hosthealth.LoadAttribution, error) {
			panic("attribution read under the limit")
		}, StatusOK, "", "1-minute load 20.00 on 14 cores"},
	} {
		t.Run(name, func(t *testing.T) {
			p := LoadProbe{Factor: 4, ReadLoad: read(tc.load), Cores: func() int { return 14 }, Attribution: tc.attribution}
			r := one(t, p.Check(context.Background()))
			if r.Status != tc.wantStatus || r.Condition != tc.wantCondition || r.Observed != tc.wantObserved {
				t.Errorf("result = %+v\nwant status %s condition %q observed %q", r, tc.wantStatus, tc.wantCondition, tc.wantObserved)
			}
		})
	}
}

// TestLoadProbe_NilAttributionIsUnchanged: with no Attribution the result is
// byte-identical to the E85.1 probe's.
func TestLoadProbe_NilAttributionIsUnchanged(t *testing.T) {
	p := LoadProbe{Factor: 4, ReadLoad: func(context.Context) (float64, error) { return 41, nil }, Cores: func() int { return 10 }}
	got := fmt.Sprintf("%+v", one(t, p.Check(context.Background())))
	want := fmt.Sprintf("%+v", Result{
		Probe: "load", Condition: ConditionHostLoadHigh, Status: StatusFailing,
		Observed: "1-minute load 41.00 on 10 cores", Threshold: "<= 40.0 (4.0 x 10 cores)",
		Detail: map[string]any{"load1": 41.0, "cores": 10, "factor": 4.0},
	})
	if got != want {
		t.Errorf("nil Attribution result\n got %s\nwant %s", got, want)
	}
}

func TestLoadParsers(t *testing.T) {
	for name, tc := range map[string]struct {
		parse func(string) (float64, error)
		in    string
		want  float64
	}{
		"proc loadavg":         {parseProcLoadavg, "1.25 0.80 0.50 2/345 6789\n", 1.25},
		"darwin sysctl":        {parseSysctlLoadavg, "{ 7.19 6.83 5.76 }\n", 7.19},
		"uptime plural (BSD)":  {parseUptimeLoad, "12:00  up 3 days, 2 users, load averages: 3.42 2.10 1.00", 3.42},
		"uptime singular":      {parseUptimeLoad, " 12:00:00 up 1 day,  load average: 0.15, 0.10, 0.05", 0.15},
		"uptime comma decimal": {parseUptimeLoad, "load average: 2,50, 1,00, 0,50", 2.5},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := tc.parse(tc.in)
			if err != nil || got != tc.want {
				t.Errorf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	for _, bad := range []func(string) (float64, error){parseProcLoadavg, parseSysctlLoadavg, parseUptimeLoad} {
		if _, err := bad(""); err == nil {
			t.Error("parser accepted empty input")
		}
	}
}

func TestReadLoad1_SourceOrder(t *testing.T) {
	noFile := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	execOut := func(outputs map[string]string) ExecFunc {
		return func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if out, ok := outputs[name]; ok {
				return []byte(out), nil
			}
			return nil, errors.New("not found")
		}
	}
	got, err := readLoad1(context.Background(), func(string) ([]byte, error) { return []byte("2.5 1 1 1/1 1"), nil }, execOut(nil))
	if err != nil || got != 2.5 {
		t.Errorf("proc: %v %v", got, err)
	}
	got, err = readLoad1(context.Background(), func(string) ([]byte, error) { return []byte("garbage"), nil },
		execOut(map[string]string{"sysctl": "{ 7.19 6.83 5.76 }"}))
	if err != nil || got != 7.19 {
		t.Errorf("sysctl after malformed proc: %v %v", got, err)
	}
	got, err = readLoad1(context.Background(), noFile, execOut(map[string]string{"uptime": "load averages: 1.50 1 1"}))
	if err != nil || got != 1.5 {
		t.Errorf("uptime: %v %v", got, err)
	}
	if _, err := readLoad1(context.Background(), noFile, execOut(nil)); err == nil {
		t.Error("no source readable returned nil error")
	}
}

// --- docker -----------------------------------------------------------------

// shExec runs a real shell script in place of the named command, so the probe
// sees a genuine *exec.ExitError with captured stderr.
func shExec(script string) ExecFunc {
	return func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		return DefaultExec(ctx, "sh", "-c", script)
	}
}

func TestDockerProbe(t *testing.T) {
	for name, tc := range map[string]struct {
		exec     ExecFunc
		timeout  time.Duration
		want     Status
		cond     string
		observed string
	}{
		"not on PATH": {func(ctx context.Context, _ string, args ...string) ([]byte, error) {
			return DefaultExec(ctx, "fishhawk-opsmonitor-no-such-docker", args...)
		}, 0, StatusUnknown, "", ""},
		"exit error":      {shExec("echo 'Cannot connect to the Docker daemon at unix:///Users/secret-user/.docker/run/docker.sock' >&2; exit 3"), 0, StatusFailing, ConditionDockerUnreachable, "docker info failed (exit 3)"},
		"stderr EIO":      {shExec("echo 'write /var/lib/docker/x: input/output error' >&2; exit 1"), 0, StatusFailing, ConditionStorageEIO, "docker info reported a storage I/O error (EIO)"},
		"error text EIO":  {func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("read: EIO") }, 0, StatusFailing, ConditionStorageEIO, "docker info reported a storage I/O error (EIO)"},
		"non-exit error":  {func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("fork failed") }, 0, StatusFailing, ConditionDockerUnreachable, "docker info failed to run"},
		"deadline":        {func(ctx context.Context, _ string, _ ...string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }, timescale.D(30 * time.Millisecond), StatusFailing, ConditionDockerUnreachable, "did not answer within"},
		"success":         {func(context.Context, string, ...string) ([]byte, error) { return []byte("27.3.1\n"), nil }, 0, StatusOK, "", ""},
		"word-bound only": {func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("PEIOX unrelated") }, 0, StatusFailing, ConditionDockerUnreachable, "docker info failed to run"},
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, DockerProbe{Exec: tc.exec, Timeout: tc.timeout}.Check(context.Background()))
			if r.Status != tc.want || r.Condition != tc.cond || !strings.Contains(r.Observed, tc.observed) {
				t.Errorf("result = %+v, want %s %s %q", r, tc.want, tc.cond, tc.observed)
			}
			if strings.Contains(r.Observed+r.Reason, "secret-user") {
				t.Errorf("raw docker error text leaked: %+v", r)
			}
		})
	}
}

func TestDockerProbe_InvokesDockerInfo(t *testing.T) {
	var got string
	DockerProbe{Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = name + " " + strings.Join(args, " ")
		return nil, nil
	}}.Check(context.Background())
	if got != "docker info --format {{.ServerVersion}}" {
		t.Errorf("exec = %q", got)
	}
}

// --- postgres ---------------------------------------------------------------

func TestPostgresProbe(t *testing.T) {
	ping := func(err error) func(context.Context) error { return func(context.Context) error { return err } }
	for name, tc := range map[string]struct {
		probe    PostgresProbe
		want     Status
		cond     string
		observed string
	}{
		"no database": {PostgresProbe{}, StatusUnknown, "", ""},
		"error":       {PostgresProbe{Ping: ping(errors.New("dial tcp db.internal:5432: connection refused"))}, StatusFailing, ConditionPostgresUnreachable, "postgres ping failed"},
		"timeout":     {PostgresProbe{Ping: ping(fmt.Errorf("ping: %w", context.DeadlineExceeded))}, StatusFailing, ConditionPostgresUnreachable, "postgres ping timed out"},
		"eio":         {PostgresProbe{Ping: ping(errors.New("could not read block: Input/output error"))}, StatusFailing, ConditionStorageEIO, "postgres ping reported a storage I/O error (EIO)"},
		"ok":          {PostgresProbe{Ping: ping(nil)}, StatusOK, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, tc.probe.Check(context.Background()))
			if r.Status != tc.want || r.Condition != tc.cond || r.Observed != tc.observed && tc.observed != "" {
				t.Errorf("result = %+v, want %s %s %q", r, tc.want, tc.cond, tc.observed)
			}
			if strings.Contains(r.Observed+r.Reason, "db.internal") {
				t.Errorf("raw pgx error text leaked: %+v", r)
			}
		})
	}
	if (PostgresProbe{}).HostLocal() {
		t.Error("postgres must not be host-local")
	}
}

// --- runners ----------------------------------------------------------------

type fakeRuns struct {
	runs      map[uuid.UUID]*run.Run
	stages    map[uuid.UUID]*run.Stage
	err       error
	nilResult bool
}

func (f *fakeRuns) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.nilResult {
		return nil, nil
	}
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, run.ErrNotFound
}

func (f *fakeRuns) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.nilResult {
		return nil, nil
	}
	if s, ok := f.stages[id]; ok {
		return s, nil
	}
	return nil, run.ErrNotFound
}

func procList(procs ...stalesweep.RunnerProcess) func(context.Context) ([]stalesweep.RunnerProcess, error) {
	return func(context.Context) ([]stalesweep.RunnerProcess, error) { return procs, nil }
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }

func TestRunnerProbe_ConfirmationAcrossSweeps(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	runs := &fakeRuns{stages: map[uuid.UUID]*run.Stage{stageID: {ID: stageID, RunID: runID, State: run.StageStateSucceeded}}}
	p := &RunnerProbe{Runs: runs, List: procList(stalesweep.RunnerProcess{PID: "4242", RunID: ptr(runID), StageID: ptr(stageID)})}
	first := one(t, p.Check(context.Background()))
	if first.Status != StatusOK || first.Detail["pending_confirmation"] != 1 {
		t.Fatalf("first Check = %+v, want ok with one pending candidate", first)
	}
	second := one(t, p.Check(context.Background()))
	if second.Status != StatusFailing || second.Condition != ConditionOrphanedRunner {
		t.Fatalf("second Check = %+v, want failing orphaned_runner", second)
	}
	for _, want := range []string{"4242", runID.String(), stageID.String(), "succeeded"} {
		if !strings.Contains(second.Observed, want) {
			t.Errorf("Observed %q missing %s", second.Observed, want)
		}
	}
	if second.Subject != "4242/"+runID.String()+"/"+stageID.String() {
		t.Errorf("Subject = %q", second.Subject)
	}
}

func TestRunnerProbe_ConfirmSweepsOneFailsImmediately(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	runs := &fakeRuns{stages: map[uuid.UUID]*run.Stage{stageID: {ID: stageID, State: run.StageStateFailed}}}
	p := &RunnerProbe{Runs: runs, ConfirmSweeps: 1, List: procList(stalesweep.RunnerProcess{PID: "1", RunID: ptr(runID), StageID: ptr(stageID)})}
	if r := one(t, p.Check(context.Background())); r.Status != StatusFailing {
		t.Errorf("result = %+v, want failing on the first Check at ConfirmSweeps=1", r)
	}
}

func TestRunnerProbe_CandidateResetsWhenAbsent(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	runs := &fakeRuns{stages: map[uuid.UUID]*run.Stage{stageID: {ID: stageID, State: run.StageStateCancelled}}}
	proc := stalesweep.RunnerProcess{PID: "7", RunID: ptr(runID), StageID: ptr(stageID)}
	present := true
	p := &RunnerProbe{Runs: runs, List: func(context.Context) ([]stalesweep.RunnerProcess, error) {
		if present {
			return []stalesweep.RunnerProcess{proc}, nil
		}
		return nil, nil
	}}
	p.Check(context.Background())
	present = false
	p.Check(context.Background())
	present = true
	if r := one(t, p.Check(context.Background())); r.Status == StatusFailing {
		t.Errorf("result = %+v, want not failing (the gap reset the candidate)", r)
	}
}

func TestRunnerProbe_NoStageIDUsesRun(t *testing.T) {
	runID := uuid.New()
	runs := &fakeRuns{runs: map[uuid.UUID]*run.Run{runID: {ID: runID, State: run.StateFailed}}}
	p := &RunnerProbe{Runs: runs, List: procList(stalesweep.RunnerProcess{PID: "9", RunID: ptr(runID)})}
	p.Check(context.Background())
	r := one(t, p.Check(context.Background()))
	if r.Status != StatusFailing || r.Subject != "9/"+runID.String()+"/-" {
		t.Errorf("result = %+v, want the run-level orphan path", r)
	}
}

// TestRunnerProbe_StatusBranches: the not-found rows pin that a runner this
// tracker cannot see (another backend, or a tenanted run hidden by row-level
// security) is unobserved, never ok, whether it is every runner or only some.
func TestRunnerProbe_StatusBranches(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	live := stalesweep.RunnerProcess{PID: "1", RunID: ptr(runID), StageID: ptr(stageID)}
	running := &fakeRuns{stages: map[uuid.UUID]*run.Stage{stageID: {ID: stageID, State: run.StageStateRunning}}}
	for name, tc := range map[string]struct {
		probe  *RunnerProbe
		want   Status
		reason string
	}{
		"no database":        {&RunnerProbe{List: procList(live)}, StatusUnknown, "no database configured"},
		"list error":         {&RunnerProbe{Runs: running, List: func(context.Context) ([]stalesweep.RunnerProcess, error) { return nil, errors.New("ps missing") }}, StatusUnknown, "process scan failed"},
		"non-terminal":       {&RunnerProbe{Runs: running, List: procList(live)}, StatusOK, ""},
		"no runners":         {&RunnerProbe{Runs: running, List: procList()}, StatusOK, ""},
		"all not found":      {&RunnerProbe{Runs: &fakeRuns{}, List: procList(live)}, StatusUnknown, "1 fishhawk-runner process(es) not found in this tracker"},
		"some not found":     {&RunnerProbe{Runs: running, List: procList(live, stalesweep.RunnerProcess{PID: "6", RunID: ptr(runID), StageID: ptr(uuid.New())})}, StatusUnknown, "1 fishhawk-runner process(es) not found in this tracker"},
		"nil stage result":   {&RunnerProbe{Runs: &fakeRuns{nilResult: true}, List: procList(live)}, StatusUnknown, "not found in this tracker"},
		"nil run result":     {&RunnerProbe{Runs: &fakeRuns{nilResult: true}, List: procList(stalesweep.RunnerProcess{PID: "2", RunID: ptr(runID)})}, StatusUnknown, "not found in this tracker"},
		"lookup error":       {&RunnerProbe{Runs: &fakeRuns{err: errors.New("db down")}, List: procList(live)}, StatusUnknown, "1 runner lookup(s) failed"},
		"run lookup error":   {&RunnerProbe{Runs: &fakeRuns{err: errors.New("db down")}, List: procList(stalesweep.RunnerProcess{PID: "2", RunID: ptr(runID)})}, StatusUnknown, "lookup(s) failed"},
		"only unattributed":  {&RunnerProbe{Runs: running, List: procList(stalesweep.RunnerProcess{PID: "3"}, stalesweep.RunnerProcess{PID: "4"})}, StatusUnknown, "2 fishhawk-runner process(es) carry no readable --run-id/--stage-id"},
		"malformed stage id": {&RunnerProbe{Runs: running, List: procList(stalesweep.RunnerProcess{PID: "5", RunID: ptr(runID), StageIDMalformed: true})}, StatusUnknown, "1 fishhawk-runner process(es) carry no readable"},
	} {
		t.Run(name, func(t *testing.T) {
			r := one(t, tc.probe.Check(context.Background()))
			if r.Status != tc.want || !strings.Contains(r.Reason, tc.reason) {
				t.Errorf("result = %+v, want %s %q", r, tc.want, tc.reason)
			}
		})
	}
}

// TestRunnerProbe_FailingWinsOverUnattributed: an unattributed runner never
// hides a confirmed orphan.
func TestRunnerProbe_FailingWinsOverUnattributed(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	runs := &fakeRuns{stages: map[uuid.UUID]*run.Stage{stageID: {ID: stageID, State: run.StageStateSucceeded}}}
	p := &RunnerProbe{Runs: runs, ConfirmSweeps: 1, List: procList(
		stalesweep.RunnerProcess{PID: "1", RunID: ptr(runID), StageID: ptr(stageID)},
		stalesweep.RunnerProcess{PID: "2"},
	)}
	if r := one(t, p.Check(context.Background())); r.Status != StatusFailing {
		t.Errorf("result = %+v, want failing", r)
	}
	if !(&RunnerProbe{}).HostLocal() || (&RunnerProbe{}).Name() != "runners" {
		t.Error("runners probe metadata")
	}
}

// --- exec -------------------------------------------------------------------

// TestExec_WaitDelayBoundsGrandchild pins the os/exec WaitDelay claim: a
// backgrounded grandchild holding stdout cannot keep DefaultExec blocked.
func TestExec_WaitDelayBoundsGrandchild(t *testing.T) {
	deadline := timescale.D(200 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	out, _ := DefaultExec(ctx, "sh", "-c", "sleep 30 & echo $!")
	elapsed := time.Since(start)
	if pid, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && pid > 0 {
		if proc, ferr := os.FindProcess(pid); ferr == nil {
			_ = proc.Kill()
		}
	}
	if limit := deadline + execWaitDelay + timescale.D(3*time.Second); elapsed > limit {
		t.Errorf("DefaultExec took %s, want <= %s", elapsed, limit)
	}
}

func TestDefaultExec_MissingBinaryIsErrNotFound(t *testing.T) {
	_, err := DefaultExec(context.Background(), "fishhawk-opsmonitor-no-such-binary")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("err = %v, want exec.ErrNotFound", err)
	}
}

// TestRunnerProbe_NonTerminalNeverConfirms: a live stage seen on many
// consecutive Checks never becomes an orphan (pins the terminal-state check
// itself, not just the confirmation count).
func TestRunnerProbe_NonTerminalNeverConfirms(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	runs := &fakeRuns{stages: map[uuid.UUID]*run.Stage{stageID: {ID: stageID, State: run.StageStateRunning}}}
	p := &RunnerProbe{Runs: runs, List: procList(stalesweep.RunnerProcess{PID: "1", RunID: ptr(runID), StageID: ptr(stageID)})}
	for i := 0; i < 3; i++ {
		if r := one(t, p.Check(context.Background())); r.Status != StatusOK {
			t.Fatalf("Check %d = %+v, want ok", i, r)
		}
	}
}

// TestGoCacheProbe_OffIsUnknownEvenWhenAnOffDirExists isolates the `off`
// guard: a relative directory literally named "off" holding a real-looking
// cache must not be measured when GOCACHE=off (caching disabled).
func TestGoCacheProbe_OffIsUnknownEvenWhenAnOffDirExists(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "off", "00"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "off", "README"), []byte(goCacheReadmeMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	r := one(t, GoCacheProbe{MaxBytes: 1 << 40, ResolveDir: func(context.Context) (string, error) { return "off", nil }}.Check(context.Background()))
	if r.Status != StatusUnknown {
		t.Errorf("result = %+v, want unknown for GOCACHE=off", r)
	}
}
