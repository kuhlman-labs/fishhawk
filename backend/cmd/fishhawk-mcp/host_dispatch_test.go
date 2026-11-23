package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
)

type hostDispatchRecorder struct {
	calls int
	cfg   mcpserver.HostDispatcherConfig
}

func testHostDispatchDeps(rec *hostDispatchRecorder, env map[string]string, now time.Time) hostDispatchDeps {
	return hostDispatchDeps{
		getenv:     func(k string) string { return env[k] },
		loadConfig: func() (config, error) { return config{backendURL: "http://fishhawkd.test", apiToken: "fhk_test"}, nil },
		run: func(_ context.Context, cfg mcpserver.HostDispatcherConfig) error {
			rec.calls++
			rec.cfg = cfg
			return nil
		},
		now: func() time.Time { return now },
	}
}

func runHD(t *testing.T, deps hostDispatchDeps, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runHostDispatch(context.Background(), args, &out, &errOut, deps)
	return code, out.String(), errOut.String()
}

func TestHostDispatch_RequiresAllowedRoots(t *testing.T) {
	rec := &hostDispatchRecorder{}
	code, _, stderr := runHD(t, testHostDispatchDeps(rec, nil, time.Now()), "--state-dir", t.TempDir())
	if code != exitUsage || !strings.Contains(stderr, "--allowed-roots") || rec.calls != 0 {
		t.Fatalf("no roots: code=%d stderr=%q loop calls=%d; want exit 2 naming --allowed-roots and no loop", code, stderr, rec.calls)
	}
	// The env fallback satisfies it, and the flag wins over the env.
	env := map[string]string{"FISHHAWK_MCP_ALLOWED_ROOTS": "/env/root"}
	if code, _, stderr := runHD(t, testHostDispatchDeps(rec, env, time.Now()), "--state-dir", t.TempDir()); code != exitOK || rec.calls != 1 {
		t.Fatalf("env roots: code=%d stderr=%q calls=%d", code, stderr, rec.calls)
	}
	if got := rec.cfg.AllowedRoots; len(got) != 1 || got[0] != "/env/root" {
		t.Fatalf("env roots = %v", got)
	}
	if code, _, _ := runHD(t, testHostDispatchDeps(rec, env, time.Now()), "--allowed-roots", "/a"+string(os.PathListSeparator)+"/b", "--state-dir", "/sd", "--interval", "20s", "--max-in-flight", "3", "--per-tick-cap", "5", "--daily-cap", "9"); code != exitOK {
		t.Fatalf("flag roots: code=%d", code)
	}
	c := rec.cfg
	if len(c.AllowedRoots) != 2 || c.AllowedRoots[0] != "/a" || c.StateDir != "/sd" || c.Interval != 20*time.Second || c.MaxInFlight != 3 || c.PerTickCap != 5 || c.DailyCap != 9 || c.BackendURL != "http://fishhawkd.test" || c.APIToken != "fhk_test" {
		t.Fatalf("loop config = %+v", c)
	}
}

// TestHostDispatch_NoDailyCapByDefault pins the flag-to-config hop of ADR-104
// A1: no --daily-cap hands the loop DailyCap 0 (no daily cap), a positive value
// is the explicit opt-in, and 0 is accepted as the same "no cap".
func TestHostDispatch_NoDailyCapByDefault(t *testing.T) {
	rec := &hostDispatchRecorder{}
	deps := testHostDispatchDeps(rec, nil, time.Now())
	if code, _, stderr := runHD(t, deps, "--allowed-roots", "/r", "--state-dir", "/sd"); code != exitOK || rec.calls != 1 {
		t.Fatalf("no --daily-cap: code=%d stderr=%q calls=%d; want the loop started", code, stderr, rec.calls)
	}
	if rec.cfg.DailyCap != 0 {
		t.Fatalf("no --daily-cap: loop DailyCap = %d; want 0 (no daily cap)", rec.cfg.DailyCap)
	}
	if code, _, stderr := runHD(t, deps, "--allowed-roots", "/r", "--state-dir", "/sd", "--daily-cap", "3"); code != exitOK || rec.calls != 2 {
		t.Fatalf("--daily-cap 3: code=%d stderr=%q calls=%d", code, stderr, rec.calls)
	}
	if rec.cfg.DailyCap != 3 {
		t.Fatalf("--daily-cap 3: loop DailyCap = %d; want 3", rec.cfg.DailyCap)
	}
	if code, _, stderr := runHD(t, deps, "--allowed-roots", "/r", "--state-dir", "/sd", "--daily-cap", "0"); code != exitOK || rec.calls != 3 {
		t.Fatalf("--daily-cap 0: code=%d stderr=%q calls=%d; want accepted", code, stderr, rec.calls)
	}
	if rec.cfg.DailyCap != 0 {
		t.Fatalf("--daily-cap 0: loop DailyCap = %d; want 0", rec.cfg.DailyCap)
	}
}

func TestHostDispatch_RefusesNegativeDailyCap(t *testing.T) {
	rec := &hostDispatchRecorder{}
	code, _, stderr := runHD(t, testHostDispatchDeps(rec, nil, time.Now()), "--allowed-roots", "/r", "--state-dir", "/sd", "--daily-cap", "-1")
	if code != exitUsage || !strings.Contains(stderr, "--daily-cap") || rec.calls != 0 {
		t.Fatalf("--daily-cap -1: code=%d stderr=%q calls=%d; want exit 2 naming --daily-cap and no loop", code, stderr, rec.calls)
	}
}

func TestHostDispatch_RefusesLongInterval(t *testing.T) {
	rec := &hostDispatchRecorder{}
	deps := testHostDispatchDeps(rec, nil, time.Now())
	code, _, stderr := runHD(t, deps, "--allowed-roots", "/r", "--interval", "46s")
	if code != exitUsage || !strings.Contains(stderr, "45s") || rec.calls != 0 {
		t.Fatalf("46s: code=%d stderr=%q calls=%d; want exit 2 and no loop", code, stderr, rec.calls)
	}
	if code, _, _ := runHD(t, deps, "--allowed-roots", "/r", "--interval", "45s"); code != exitOK || rec.calls != 1 {
		t.Fatalf("45s: code=%d calls=%d; want the loop started", code, rec.calls)
	}
	if code, _, _ := runHD(t, deps, "--allowed-roots", "/r", "--max-in-flight", "0"); code != exitUsage {
		t.Fatalf("max-in-flight 0: code=%d; want exit 2", code)
	}
}

func writeStatus(t *testing.T, dir string, st mcpserver.HostDispatcherStatus) {
	t.Helper()
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "status.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostDispatch_Status(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	deps := testHostDispatchDeps(&hostDispatchRecorder{}, nil, now)
	dir := t.TempDir()
	if code, _, stderr := runHD(t, deps, "--status", "--state-dir", dir); code != exitFailure || !strings.Contains(stderr, "no dispatcher status") {
		t.Fatalf("absent: code=%d stderr=%q; want 1", code, stderr)
	}
	writeStatus(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-60 * time.Second), IntervalSeconds: 30, LastRefusalCode: "cap_reached:daily"})
	if code, out, _ := runHD(t, deps, "--status", "--state-dir", dir); code != exitOK || !strings.Contains(out, "state=live") || !strings.Contains(out, "last_refusal=cap_reached:daily") {
		t.Fatalf("live: code=%d out=%q; want 0 state=live", code, out)
	}
	// A status without an opt-in daily cap reports spawns_today as an
	// observation and names no daily limit; with one set, daily_cap=N shows. Both
	// fixtures are the real HostDispatcherStatus the loop writes, so the
	// daily_cap tag round-trips from writer to renderer.
	writeStatus(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-60 * time.Second), IntervalSeconds: 30, SpawnsToday: 50})
	if code, out, _ := runHD(t, deps, "--status", "--state-dir", dir); code != exitOK || !strings.Contains(out, "spawns_today=50") || strings.Contains(out, "daily_cap") {
		t.Fatalf("no daily cap: code=%d out=%q; want spawns_today=50 and no daily_cap token", code, out)
	}
	writeStatus(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-60 * time.Second), IntervalSeconds: 30, SpawnsToday: 2, DailyCap: 3})
	if code, out, _ := runHD(t, deps, "--status", "--state-dir", dir); code != exitOK || !strings.Contains(out, "spawns_today=2") || !strings.Contains(out, "daily_cap=3") {
		t.Fatalf("daily cap 3: code=%d out=%q; want spawns_today=2 daily_cap=3", code, out)
	}
	writeStatus(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-60 * time.Second), IntervalSeconds: 30, Paused: true})
	if code, out, _ := runHD(t, deps, "--status", "--state-dir", dir); code != exitOK || !strings.Contains(out, "state=paused") {
		t.Fatalf("paused: code=%d out=%q; want 0 state=paused", code, out)
	}
	writeStatus(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-91 * time.Second), IntervalSeconds: 30})
	if code, out, _ := runHD(t, deps, "--status", "--state-dir", dir); code != exitFailure || !strings.Contains(out, "state=stale") {
		t.Fatalf("stale: code=%d out=%q; want 1 state=stale", code, out)
	}
}

func TestHostDispatch_PauseResume(t *testing.T) {
	deps := testHostDispatchDeps(&hostDispatchRecorder{}, nil, time.Now())
	dir := filepath.Join(t.TempDir(), "state")
	if code, _, stderr := runHD(t, deps, "--pause", "--state-dir", dir); code != exitOK {
		t.Fatalf("pause: code=%d stderr=%q", code, stderr)
	}
	fi, err := os.Stat(filepath.Join(dir, "pause"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pause file: %v mode=%v; want 0600", err, fi)
	}
	if code, _, _ := runHD(t, deps, "--pause", "--state-dir", dir); code != exitOK {
		t.Fatalf("second pause: code=%d; want idempotent 0", code)
	}
	if code, _, _ := runHD(t, deps, "--resume", "--state-dir", dir); code != exitOK {
		t.Fatalf("resume: code=%d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "pause")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resume left the pause file: %v", err)
	}
	if code, _, _ := runHD(t, deps, "--pause", "--resume", "--state-dir", dir); code != exitUsage {
		t.Fatalf("--pause --resume: code=%d; want 2", code)
	}
}

// TestRun_RoutesHostDispatchSubcommand pins that run() hands `host-dispatch`
// to the subcommand BEFORE the transport flag parse: the subcommand's own
// interval bound answers exit 2 here, where the transport parser would have
// ignored the positional and started a stdio server.
func TestRun_RoutesHostDispatchSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"fishhawk-mcp", "host-dispatch", "--allowed-roots", "/r", "--interval", "46s"}, &out, &errOut)
	if code != exitUsage || !strings.Contains(errOut.String(), "--interval 46s") {
		t.Fatalf("routed run: code=%d stderr=%q; want the subcommand's exit 2", code, errOut.String())
	}
	if !isHostDispatchInvocation([]string{"fishhawk-mcp", "host-dispatch"}) || isHostDispatchInvocation([]string{"fishhawk-mcp", "--transport", "http"}) || isHostDispatchInvocation([]string{"fishhawk-mcp"}) {
		t.Fatal("isHostDispatchInvocation misclassified an invocation")
	}
}

// writeStatusWith writes st plus extra raw keys into dir/status.json. The
// #5402 element types (run_refusals entries, last_tick_failure) are
// unexported in mcpserver, so a fixture carries them as the JSON the
// dispatcher writes.
func writeStatusWith(t *testing.T, dir string, st mcpserver.HostDispatcherStatus, extra map[string]any) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Item 10: a fresh status with two per-run refusals prints run_refusals=2.
func TestHostDispatchStatus_PrintsRunRefusalCount(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	entry := func(run, code string) map[string]any {
		return map[string]any{"run_id": run, "stage_id": "s-" + run, "code": code, "first_at": now.Add(-time.Minute), "last_at": now.Add(-30 * time.Second)}
	}
	writeStatusWith(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-30 * time.Second), LastCompleteTickAt: now.Add(-30 * time.Second), IntervalSeconds: 30},
		map[string]any{"run_refusals": map[string]any{"a": entry("a", "working_dir_absent"), "b": entry("b", "cap_reached:daily")}})
	code, out, stderr := runHD(t, testHostDispatchDeps(&hostDispatchRecorder{}, nil, now), "--status", "--state-dir", dir)
	if code != exitOK || !strings.Contains(out, "state=live") || !strings.Contains(out, " run_refusals=2") {
		t.Fatalf("code=%d out=%q stderr=%q; want 0, state=live and run_refusals=2", code, out, stderr)
	}
	if !strings.Contains(out, " last_complete_tick_at="+now.Add(-30*time.Second).Format(time.RFC3339)) {
		t.Fatalf("out=%q; want last_complete_tick_at", out)
	}
}

// Item 11 (R2): a ticking dispatcher whose latest tick failed prints
// state=failing with the failure code and exits 1.
func TestHostDispatchStatus_FailingTickExitsOne(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeStatusWith(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-10 * time.Second), LastCompleteTickAt: now.Add(-40 * time.Second), IntervalSeconds: 30},
		map[string]any{"last_tick_failure": map[string]any{"code": "list_runs_failed", "at": now.Add(-10 * time.Second)}})
	code, out, _ := runHD(t, testHostDispatchDeps(&hostDispatchRecorder{}, nil, now), "--status", "--state-dir", dir)
	if code != exitFailure || !strings.Contains(out, "state=failing") || !strings.Contains(out, " tick_failure=list_runs_failed at="+now.Add(-10*time.Second).Format(time.RFC3339)) {
		t.Fatalf("code=%d out=%q; want 1, state=failing and tick_failure=list_runs_failed", code, out)
	}
}

// Item 12: FISHHAWK_HOST_DISPATCH_STATE_DIR is the --state-dir default.
// TMPDIR points at a temp dir FIRST, so a reverted default never touches the
// host's real $TMPDIR/fishhawk-host-dispatch.
func TestHostDispatch_StateDirDefaultsToEnv(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	envDir := filepath.Join(t.TempDir(), "env-state")
	deps := testHostDispatchDeps(&hostDispatchRecorder{}, map[string]string{"FISHHAWK_HOST_DISPATCH_STATE_DIR": envDir}, time.Now())
	if code, _, stderr := runHD(t, deps, "--pause"); code != exitOK {
		t.Fatalf("--pause: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(envDir, "pause")); err != nil {
		t.Fatalf("pause file not in the env dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "fishhawk-host-dispatch", "pause")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pause file landed in the TMPDIR default: %v", err)
	}
}

// Item 15 (S3): a status file a pre-#5402 dispatcher wrote (no
// last_complete_tick_at, last_tick_failure or run_refusals) prints today's
// line: state=live, no new token, exit 0.
func TestHostDispatchStatus_PreChangeFileReadsLive(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeStatus(t, dir, mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-60 * time.Second), IntervalSeconds: 30, SpawnsToday: 5, SpawnsDay: now.Format(time.DateOnly)})
	code, out, _ := runHD(t, testHostDispatchDeps(&hostDispatchRecorder{}, nil, now), "--status", "--state-dir", dir)
	if code != exitOK || !strings.Contains(out, "state=live") {
		t.Fatalf("pre-change file: code=%d out=%q; want 0 state=live", code, out)
	}
	for _, tok := range []string{"last_complete_tick_at=", "tick_failure=", "run_refusals="} {
		if strings.Contains(out, tok) {
			t.Fatalf("pre-change file printed %q: %q", tok, out)
		}
	}
}

// H4 (ADR-030 Amendment A1 / #5473): --status prints the summary line
// byte-unchanged, then one bounded budget_blocked line per entry (sorted by
// run id) naming its budget source; other refusal codes print no line and
// the exit code is unchanged.
func TestHostDispatchStatus_PrintsBudgetBlockedRefusals(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := mcpserver.HostDispatcherStatus{PID: 7, LastTickAt: now.Add(-30 * time.Second), LastCompleteTickAt: now.Add(-30 * time.Second), IntervalSeconds: 30}
	entry := func(run, code, detail string) map[string]any {
		return map[string]any{"run_id": run, "stage_id": "s-" + run, "code": code, "detail": detail, "first_at": now.Add(-time.Minute), "last_at": now.Add(-30 * time.Second)}
	}
	live := "blocking weekly budget of workflow feature_change is over its limit: spent $15.00 of $10.00 in 2026-11-16T00:00:00Z..2026-11-23T00:00:00Z (budget_source live_spec); spawns on the next tick"
	crafted := "\x1b[31m\nspent $15.00 of $10.00 (budget_source admission_spec)" + strings.Repeat("d", 2000)
	refusals := map[string]any{
		"b": entry("b", "budget_blocked", crafted),
		"a": entry("a", "budget_blocked", live),
		"c": entry("c", "working_dir_absent", ""),
	}

	// The summary line with the same refusal count and no budget entry is the
	// pre-change output.
	dir0 := t.TempDir()
	writeStatusWith(t, dir0, st, map[string]any{"run_refusals": map[string]any{
		"a": entry("a", "working_dir_absent", ""), "b": entry("b", "working_dir_absent", ""), "c": entry("c", "working_dir_absent", "")}})
	_, base, _ := runHD(t, testHostDispatchDeps(&hostDispatchRecorder{}, nil, now), "--status", "--state-dir", dir0)
	if strings.Count(base, "\n") != 1 {
		t.Fatalf("baseline out=%q; want exactly the summary line", base)
	}

	dir := t.TempDir()
	writeStatusWith(t, dir, st, map[string]any{"run_refusals": refusals})
	code, out, stderr := runHD(t, testHostDispatchDeps(&hostDispatchRecorder{}, nil, now), "--status", "--state-dir", dir)
	if code != exitOK {
		t.Fatalf("code=%d stderr=%q; want 0 (a budget hold is not a dispatcher failure)", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("out=%q; want the summary line plus two budget_blocked lines", out)
	}
	if lines[0]+"\n" != base {
		t.Errorf("summary line = %q; want it byte-unchanged %q", lines[0], base)
	}
	since := now.Add(-time.Minute).Format(time.RFC3339)
	if want := "budget_blocked run=a stage=s-a since=" + since + " source=live_spec detail=" + live; lines[1] != want {
		t.Errorf("line 1 = %q; want %q", lines[1], want)
	}
	if want := "budget_blocked run=b stage=s-b since=" + since + " source=admission_spec detail=[31mspent $15.00 of $10.00"; !strings.HasPrefix(lines[2], want) {
		t.Errorf("line 2 = %q; want prefix %q", lines[2], want)
	}
	if strings.ContainsAny(lines[2], "\x1b\r") || strings.Count(lines[2], "d") > 300 {
		t.Errorf("line 2 is unbounded or carries a control rune: %q", lines[2])
	}
	if strings.Contains(out, "run=c") {
		t.Errorf("out=%q; a non-budget refusal must print no budget line", out)
	}
}
