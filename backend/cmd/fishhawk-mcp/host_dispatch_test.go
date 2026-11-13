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
