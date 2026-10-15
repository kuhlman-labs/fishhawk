package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// ---------------------------------------------------------------------------
// Bounded, credential-free image checks for the docker-gated fixtures
// (#4337 point 4).
//
// requireGateImage's pull and binary check used to run as a bare
// exec.CommandContext(...).CombinedOutput() under the OPERATOR's docker
// config. On Docker Desktop with a locked screen that pull blocks in the
// credsStore chain (docker → docker-credential-desktop →
// docker-credential-osxkeychain, AGENTS.md's locked-screen trap), and its
// 5-minute deadline could not unblock it: the default Cmd.Cancel kills only
// the direct child, and without Cmd.WaitDelay CombinedOutput waits for EOF on
// the output pipe, which the surviving helper grandchild still holds
// (https://pkg.go.dev/os/exec#Cmd.WaitDelay). The fixture then ran until the
// test binary's own 10-minute timeout. runImageCheckCmd bounds the exec;
// gateTestImageEnv takes the credential helper out of the pull entirely.
// ---------------------------------------------------------------------------

// imageCheckWaitDelay bounds how long runImageCheckCmd waits for the output
// pipe to close once the process exited or its context ended.
var imageCheckWaitDelay = scaledD(2 * time.Second)

// runImageCheckCmd runs argv (argv[0] resolved on PATH) under env (nil
// inherits the process env) and returns its trimmed combined output. The
// child leads its own process group, and a context deadline kills that WHOLE
// group, so a credential helper the CLI spawned dies with it. WaitDelay then
// closes the output pipe even when a grandchild outside the kill still holds
// it, so the call returns within ctx's deadline plus imageCheckWaitDelay.
func runImageCheckCmd(ctx context.Context, argv, env []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("runImageCheckCmd: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = imageCheckWaitDelay
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// gateTestImageEnv returns the process env re-bound the way the runner binds
// its own post-selection runtime calls (#4046): every endpoint and config
// override dropped, the validated socket pinned (DOCKER_HOST / CONTAINER_HOST)
// and DOCKER_CONFIG pinned to a freshly minted credential-free config
// (gateiso.NewAnonymousDockerConfig: the fishhawk.invalid placeholder auths
// entry, no credsStore, no credHelpers, the operator's plugin dirs carried
// forward). cleanup removes the minted dir; it is never nil and is safe to
// call on the error path.
func gateTestImageEnv(rt gateiso.Runtime) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	cfg, err := gateiso.NewAnonymousDockerConfig(gateiso.OperatorPluginDirs(os.Getenv))
	if err != nil {
		return nil, cleanup, err
	}
	env, err = rt.BindEndpointEnv(os.Environ(), cfg.Dir)
	if err != nil {
		_ = cfg.Remove()
		return nil, cleanup, err
	}
	return env, func() { _ = cfg.Remove() }, nil
}

// readPidFile polls for the pid a fixture script wrote, failing after bound.
func readPidFile(t *testing.T, path string, bound time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid recorded in %s within %s", path, bound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killOnCleanup SIGKILLs a backgrounded fixture process at test end so no
// orphan outlives the test (the #3663 starved-host trap).
func killOnCleanup(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// processGone reports whether pid stopped existing within bound (ESRCH).
func processGone(pid int, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunImageCheckCmd_GrandchildHoldingPipeReturns: the parent exits at once
// while a backgrounded sleep inherits its stdout, the shape of a credential
// helper outliving the CLI that spawned it. Only WaitDelay closes that pipe,
// so the call returns the parent's output inside the bound; without it
// CombinedOutput waits for the sleep's EOF and the outer timer fails the test.
func TestRunImageCheckCmd_GrandchildHoldingPipeReturns(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	killOnCleanup(t, pidFile)
	hold := strconv.Itoa(int(scaledD(60*time.Second) / time.Second))
	argv := []string{"sh", "-c", "sleep " + hold + " & echo $! > '" + pidFile + "'; echo started"}

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		out, err := runImageCheckCmd(context.Background(), argv, nil)
		done <- result{out, err}
	}()
	bound := scaledD(10 * time.Second)
	select {
	case r := <-done:
		if r.out != "started" {
			t.Fatalf("output = %q, want %q", r.out, "started")
		}
		if r.err != nil && !errors.Is(r.err, exec.ErrWaitDelay) {
			t.Fatalf("err = %v, want nil or exec.ErrWaitDelay", r.err)
		}
		if elapsed := time.Since(start); elapsed > bound {
			t.Fatalf("returned after %s, over the %s bound", elapsed, bound)
		}
	case <-time.After(bound):
		t.Fatalf("runImageCheckCmd still blocked after %s: a grandchild holding the output pipe outlives the call (WaitDelay unset?)", bound)
	}
}

// TestRunImageCheckCmd_DeadlineKillsProcessGroup: a context deadline must kill
// the WHOLE process group, the backgrounded sleep included. The default
// Cmd.Cancel kills only sh, so with the group kill deleted the recorded sleep
// pid stays alive and the ESRCH poll fails.
func TestRunImageCheckCmd_DeadlineKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	killOnCleanup(t, pidFile)
	hold := strconv.Itoa(int(scaledD(60*time.Second) / time.Second))
	argv := []string{"sh", "-c", "sleep " + hold + " & echo $! > '" + pidFile + "'; wait"}

	ctx, cancel := context.WithTimeout(context.Background(), scaledD(time.Second))
	defer cancel()
	start := time.Now()
	_, err := runImageCheckCmd(ctx, argv, nil)
	if err == nil {
		t.Fatal("runImageCheckCmd returned nil past its context deadline")
	}
	if elapsed, bound := time.Since(start), scaledD(10*time.Second); elapsed > bound {
		t.Fatalf("returned after %s, over the %s bound", elapsed, bound)
	}
	pid := readPidFile(t, pidFile, scaledD(5*time.Second))
	if !processGone(pid, scaledD(5*time.Second)) {
		t.Fatalf("backgrounded sleep (pid %d) survived the deadline: the cancel killed only the direct child, not its process group", pid)
	}
}

// TestRunImageCheckCmd_EmptyArgv: an empty argv is refused without an exec.
func TestRunImageCheckCmd_EmptyArgv(t *testing.T) {
	if _, err := runImageCheckCmd(context.Background(), nil, nil); err == nil {
		t.Fatal("runImageCheckCmd(nil argv) = nil error, want a refusal")
	}
}

// envValues returns every value of key in env, in order.
func envValues(env []string, key string) []string {
	var vals []string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			vals = append(vals, v)
		}
	}
	return vals
}

// TestGateTestImageEnv_IsCredentialFree: the AMBIENT environment names a
// blocking credsStore config and a different endpoint by construction, so an
// env that inherited either fails here. The bound env must pin the validated
// socket and a freshly minted config holding only the placeholder auths entry.
func TestGateTestImageEnv_IsCredentialFree(t *testing.T) {
	ambient := t.TempDir()
	if err := os.WriteFile(filepath.Join(ambient, "config.json"), []byte(`{"credsStore":"desktop","auths":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", ambient)
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2376")
	t.Setenv("DOCKER_CONTEXT", "ambient-context")
	t.Setenv("CONTAINER_HOST", "tcp://10.0.0.5:2376")
	t.Setenv("REGISTRY_AUTH_FILE", filepath.Join(ambient, "config.json"))
	sock := filepath.Join(t.TempDir(), "docker.sock")

	for _, tc := range []struct {
		kind     gateiso.Kind
		hostVar  string
		authFile bool
	}{
		{gateiso.KindDocker, "DOCKER_HOST", false},
		{gateiso.KindPodman, "CONTAINER_HOST", true},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			env, cleanup, err := gateTestImageEnv(gateiso.Runtime{Kind: tc.kind, Safe: true, SocketPath: sock})
			if err != nil {
				t.Fatalf("gateTestImageEnv: %v", err)
			}
			if got := envValues(env, tc.hostVar); len(got) != 1 || got[0] != "unix://"+sock {
				t.Fatalf("%s = %q, want exactly [unix://%s]", tc.hostVar, got, sock)
			}
			if got := envValues(env, "DOCKER_CONTEXT"); len(got) != 0 {
				t.Fatalf("DOCKER_CONTEXT survived the binding: %q", got)
			}
			cfgDirs := envValues(env, "DOCKER_CONFIG")
			if len(cfgDirs) != 1 || cfgDirs[0] == ambient || !filepath.IsAbs(cfgDirs[0]) {
				t.Fatalf("DOCKER_CONFIG = %q, want exactly one minted dir (not the ambient %s)", cfgDirs, ambient)
			}
			cfgDir := cfgDirs[0]
			if auth := envValues(env, "REGISTRY_AUTH_FILE"); tc.authFile != (len(auth) == 1 && auth[0] == filepath.Join(cfgDir, "config.json")) {
				t.Fatalf("REGISTRY_AUTH_FILE = %q (podman wants exactly the minted config.json, docker none)", auth)
			}
			raw, err := os.ReadFile(filepath.Join(cfgDir, "config.json"))
			if err != nil {
				t.Fatalf("read minted config: %v", err)
			}
			var cfg map[string]json.RawMessage
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatalf("minted config %s: %v", raw, err)
			}
			for _, key := range []string{"credsStore", "credHelpers"} {
				if _, ok := cfg[key]; ok {
					t.Fatalf("minted config carries %s: %s", key, raw)
				}
			}
			var auths map[string]map[string]any
			if err := json.Unmarshal(cfg["auths"], &auths); err != nil || len(auths) != 1 || auths["fishhawk.invalid"] == nil || len(auths["fishhawk.invalid"]) != 0 {
				t.Fatalf("minted auths = %s, want exactly {\"fishhawk.invalid\":{}}", cfg["auths"])
			}
			cleanup()
			if _, err := os.Stat(cfgDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup left the minted config dir %s: %v", cfgDir, err)
			}
		})
	}
}

// TestGateTestImageEnv_BindFailureRemovesMintedConfig: a runtime with no
// validated socket cannot be bound; the minted dir must not leak. TMPDIR is a
// fresh dir by construction, so any leftover entry is the leaked config.
func TestGateTestImageEnv_BindFailureRemovesMintedConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	env, cleanup, err := gateTestImageEnv(gateiso.Runtime{Kind: gateiso.KindDocker, Safe: true})
	if err == nil || env != nil {
		t.Fatalf("gateTestImageEnv(no socket) = %q, %v; want a refusal", env, err)
	}
	cleanup()
	entries, rerr := os.ReadDir(tmp)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("bind failure leaked %d entries under TMPDIR (first %s)", len(entries), entries[0].Name())
	}
}

// TestGateTestImageEnv_MintFailure: an unusable TMPDIR fails the mint, and
// THAT error is returned (not the downstream bind refusal of an empty config
// dir, which a missing mint check would surface instead); cleanup is still
// callable.
func TestGateTestImageEnv_MintFailure(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
	env, cleanup, err := gateTestImageEnv(gateiso.Runtime{Kind: gateiso.KindDocker, Safe: true, SocketPath: "/var/run/docker.sock"})
	if err == nil || env != nil || !strings.Contains(err.Error(), "create runner docker config dir") {
		t.Fatalf("gateTestImageEnv(unusable TMPDIR) = %q, %v; want the mint error", env, err)
	}
	cleanup()
}
