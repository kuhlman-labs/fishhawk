package mcpserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// host_dispatch_lineage_lock_test.go pins the host dispatcher's lineage-lock
// probe (#5485): the argv and input mapping, every verdict mode over a fake
// runner script, and the real fishhawk-runner's JSON contract end to end.

const (
	hdllRunID    = "cccccccc-1111-2222-3333-444444444444"
	hdllParentID = "dddddddd-5555-6666-7777-888888888888"
)

func TestHostDispatchLineageLockArgv(t *testing.T) {
	base := []string{"lineage-lock-status", "--working-dir", "/w", "--run-id", hdllRunID}
	for _, tc := range []struct {
		name string
		in   hostDispatchLineageLockInput
		want []string
	}{
		{"solo", hostDispatchLineageLockInput{WorkingDir: "/w", RunID: hdllRunID}, base},
		{"decomposed", hostDispatchLineageLockInput{WorkingDir: "/w", RunID: hdllRunID, DecomposedFrom: hdllParentID},
			append(append([]string(nil), base...), "--decomposed-from", hdllParentID)},
		{"decomposed and isolated", hostDispatchLineageLockInput{WorkingDir: "/w", RunID: hdllRunID, DecomposedFrom: hdllParentID, ParallelIsolate: true},
			append(append([]string(nil), base...), "--decomposed-from", hdllParentID, "--parallel-isolate")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostDispatchLineageLockArgv(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argv = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestHostDispatchLineageLockInputFor(t *testing.T) {
	solo := &Run{ID: hdllRunID, WorkingDir: "/w"}
	if got, want := hostDispatchLineageLockInputFor(solo, false), (hostDispatchLineageLockInput{WorkingDir: "/w", RunID: hdllRunID}); got != want {
		t.Fatalf("solo input = %+v; want %+v", got, want)
	}
	parent := hdllParentID
	child := &Run{ID: hdllRunID, WorkingDir: "/w", DecomposedFrom: &parent}
	if got, want := hostDispatchLineageLockInputFor(child, true), (hostDispatchLineageLockInput{WorkingDir: "/w", RunID: hdllRunID, DecomposedFrom: parent, ParallelIsolate: true}); got != want {
		t.Fatalf("child input = %+v; want %+v", got, want)
	}
	if got := hostDispatchLineageLockInputFor(child, false); got.ParallelIsolate || got.DecomposedFrom != parent {
		t.Fatalf("decomposed non-child-spawn input = %+v; want the parent and no isolate", got)
	}
}

// hdllFakeRunner writes a POSIX script standing in for fishhawk-runner.
func hdllFakeRunner(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fishhawk-runner")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestProbeHostDispatchLineageLock_Verdicts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fake runner")
	}
	in := hostDispatchLineageLockInput{WorkingDir: "/w", RunID: hdllRunID}
	env := []string{"PATH=" + os.Getenv("PATH")}
	for _, tc := range []struct {
		name    string
		body    string
		held    bool
		pid     int
		errHas  []string
		timeout time.Duration
	}{
		{name: "held", body: `echo '{"root":"cccccccc","lock_path":"/l.lock","present":true,"held":true,"holder_pid":77,"holder_run_id":"r"}'`, held: true, pid: 77},
		{name: "free", body: `echo '{"root":"cccccccc","lock_path":"/l.lock","present":false,"held":false,"holder_pid":0,"holder_run_id":""}'`},
		{name: "exit 2 is a runner without the subcommand", body: "echo 'flag provided but not defined: -working-dir' >&2\nexit 2",
			errHas: []string{"exited 2 (usage)", "does not support the lineage-lock-status subcommand", "deploy a fishhawk-runner that supports lineage-lock-status", "flag provided but not defined"}},
		{name: "exit 1 is not a git checkout", body: "echo '{\"event\":\"lineage_lock_status_failed\"}' >&2\nexit 1",
			errHas: []string{"exited 1", "not a git checkout", `working_dir "/w"`, "lineage_lock_status_failed"}},
		{name: "other exit", body: "exit 3", errHas: []string{"exited 3"}},
		{name: "malformed JSON", body: `echo 'not json'`, errHas: []string{"not a JSON verdict"}},
		{name: "held absent", body: `echo '{"root":"cccccccc","lock_path":"/l.lock","present":true,"holder_pid":77}'`, errHas: []string{"no held value"}},
		{name: "held null", body: `echo '{"root":"cccccccc","lock_path":"/l.lock","present":true,"held":null,"holder_pid":77}'`, errHas: []string{"no held value"}},
		{name: "empty lock_path", body: `echo '{"root":"cccccccc","lock_path":"","present":false,"held":false}'`, errHas: []string{"empty lock_path"}},
		{name: "timeout", body: "exec sleep 5", timeout: 200 * time.Millisecond, errHas: []string{"timed out after 200ms"}},
		{name: "oversized stdout", body: `printf '{"held":false,"lock_path":"/l.lock","pad":"'; head -c 70000 /dev/zero | tr '\0' 'x'; echo '"}'`,
			errHas: []string{"output exceeded 65536 bytes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.timeout > 0 {
				orig := hostDispatchLineageLockProbeTimeout
				hostDispatchLineageLockProbeTimeout = tc.timeout
				t.Cleanup(func() { hostDispatchLineageLockProbeTimeout = orig })
			}
			start := time.Now()
			got := probeHostDispatchLineageLock(context.Background(), hdllFakeRunner(t, tc.body), env, in)
			if len(tc.errHas) > 0 {
				if got.Err == nil {
					t.Fatalf("verdict = %+v; want an error", got)
				}
				for _, s := range tc.errHas {
					if !strings.Contains(got.Err.Error(), s) {
						t.Errorf("error %q does not name %q", got.Err, s)
					}
				}
				if tc.timeout > 0 && time.Since(start) > 3*time.Second {
					t.Errorf("timed-out probe took %s; want it bounded near the timeout", time.Since(start))
				}
				return
			}
			if got.Err != nil || got.Held != tc.held || got.HolderPID != tc.pid || got.LockPath != "/l.lock" || got.Root != "cccccccc" {
				t.Fatalf("verdict = %+v; want held %v pid %d lock /l.lock", got, tc.held, tc.pid)
			}
		})
	}
}

// An absent runner binary is an exec error, not a verdict.
func TestProbeHostDispatchLineageLock_ExecFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-fishhawk-runner")
	got := probeHostDispatchLineageLock(context.Background(), missing, nil, hostDispatchLineageLockInput{WorkingDir: "/w", RunID: hdllRunID})
	if got.Err == nil || got.Held || !strings.Contains(got.Err.Error(), "could not exec") {
		t.Fatalf("missing binary = %+v; want a could-not-exec error", got)
	}
}

// TestProbeHostDispatchLineageLock_RealRunner crosses the module boundary: it
// builds the REAL fishhawk-runner and drives the production probe against a
// `git init` checkout, writing the lock at the path the RUNNER reported, so a
// drift in the subcommand name, flag names, JSON field names or the path rule
// reads RED.
func TestProbeHostDispatchLineageLock_RealRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the fishhawk-runner binary")
	}
	if runtime.GOOS == "windows" {
		t.Skip("signal-0 liveness is POSIX")
	}
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	runnerDir := filepath.Join(pkgSrcDir, "..", "..", "..", "runner", "cmd", "fishhawk-runner")
	bin := filepath.Join(t.TempDir(), "fishhawk-runner")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = runnerDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fishhawk-runner: %v\n%s", err, out)
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env := os.Environ()
	in := hostDispatchLineageLockInput{WorkingDir: repo, RunID: hdllRunID}
	probe := func(what string) hostDispatchLineageLock {
		t.Helper()
		got := probeHostDispatchLineageLock(ctx, bin, env, in)
		if got.Err != nil {
			t.Fatalf("%s: probe error: %v", what, got.Err)
		}
		return got
	}

	free := probe("no lock")
	if free.Held || !strings.HasPrefix(free.LockPath, common+string(filepath.Separator)) || free.Root != hdllRunID[:8] {
		t.Fatalf("no lock = %+v; want not held, a lock_path under %s, root %s", free, common, hdllRunID[:8])
	}
	if err := os.MkdirAll(filepath.Dir(free.LockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(free.LockPath, []byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), hdllRunID)), 0o644); err != nil {
		t.Fatal(err)
	}
	if held := probe("live lock"); !held.Held || held.HolderPID != os.Getpid() || held.HolderRunID != hdllRunID || held.LockPath != free.LockPath {
		t.Fatalf("live lock = %+v; want held by pid %d run %s at %s", held, os.Getpid(), hdllRunID, free.LockPath)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(free.LockPath, []byte(fmt.Sprintf("%d\n%s\n", dead.ProcessState.Pid(), hdllRunID)), 0o644); err != nil {
		t.Fatal(err)
	}
	if stale := probe("dead pid"); stale.Held || stale.HolderPID != dead.ProcessState.Pid() {
		t.Fatalf("dead pid lock = %+v; want not held, holder_pid %d", stale, dead.ProcessState.Pid())
	}
	if err := os.Remove(free.LockPath); err != nil {
		t.Fatal(err)
	}
	if gone := probe("removed lock"); gone.Held {
		t.Fatalf("removed lock = %+v; want not held", gone)
	}
	in = hostDispatchLineageLockInput{WorkingDir: repo, RunID: hdllRunID, DecomposedFrom: hdllParentID, ParallelIsolate: true}
	if child := probe("isolated child"); child.Root != hdllRunID[:8] {
		t.Fatalf("isolated child root = %q; want its own short id %s", child.Root, hdllRunID[:8])
	}
	in.ParallelIsolate = false
	if shared := probe("shared-lineage child"); shared.Root != hdllParentID[:8] {
		t.Fatalf("shared-lineage child root = %q; want the parent's short id %s", shared.Root, hdllParentID[:8])
	}
}
