package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// ---------------------------------------------------------------------------
// Gate isolation wiring (ADR-063 / #2134): configuration, selection, the
// out-of-band gate disposition (#3448), the exec-path switch, and the clone
// materialization at the two gate sites. The gateiso package's own tests pin
// the pure pieces; these pin that the runner reaches them through its ONE
// gate-exec seam and classifies on the disposition, never on output text.
// ---------------------------------------------------------------------------

// installGateState installs st as the process-wide isolation state for the
// duration of the test and restores the nil (host exec) state afterwards.
func installGateState(t *testing.T, st *gateIsolationState) {
	t.Helper()
	prev := gateIsolation
	gateIsolation = st
	t.Cleanup(func() { gateIsolation = prev })
}

// captureHostExec swaps the host-exec seam for a recorder. When fail is
// true, any call fails the test — the shape the refusal / mount-guard tests
// need to prove nothing executed.
func captureHostExec(t *testing.T, fail bool, code int) *[][]string {
	t.Helper()
	prev := execBoundedHostArgvFn
	var calls [][]string
	execBoundedHostArgvFn = func(_ context.Context, argv []string, _ string, _ []string, _ time.Duration) (string, int) {
		calls = append(calls, append([]string(nil), argv...))
		if fail {
			t.Errorf("execBoundedHostArgvFn must not be reached; called with %q", argv)
		}
		return "captured", code
	}
	t.Cleanup(func() { execBoundedHostArgvFn = prev })
	return &calls
}

func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func unsafeRuntime() gateiso.Runtime {
	return gateiso.Runtime{Kind: gateiso.KindDocker, Safe: false,
		Reason:   "docker endpoint tcp://10.0.0.5:2376 is not a local unix socket",
		Endpoint: gateiso.Endpoint{Raw: "tcp://10.0.0.5:2376", Scheme: "tcp", Reason: "remote"}}
}

func safeDockerRuntime(sock string) gateiso.Runtime {
	return gateiso.Runtime{Kind: gateiso.KindDocker, Safe: true, Reason: "local unix socket",
		Endpoint:   gateiso.Endpoint{Raw: "unix://" + sock, Scheme: "unix", Path: sock, Resolved: sock, Local: true},
		SocketPath: sock}
}

// refusedState is a hosted+auto state whose probes report an unsafe runtime
// and no sandbox: Select refuses.
func refusedState(logSink io.Writer) *gateIsolationState {
	st, err := configureGateIsolation(fakeEnv(map[string]string{deploymentProfileEnvVar: "hosted"}), gateiso.Probes{}, logSink)
	if err != nil {
		panic(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return unsafeRuntime() }
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	return st
}

// containerState is a mode=container state whose probes report a safe docker
// runtime at sock and the given image: Select picks the container path.
func containerState(image, sock string, logSink io.Writer) *gateIsolationState {
	st, err := configureGateIsolation(fakeEnv(map[string]string{
		gateIsolationModeEnvVar: "container", gateImageEnvVar: image}), gateiso.Probes{}, logSink)
	if err != nil {
		panic(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return safeDockerRuntime(sock) }
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	return st
}

func TestConfigureGateIsolation_Rows(t *testing.T) {
	rows := []struct {
		name    string
		env     map[string]string
		wantErr []string // substrings; empty = success
	}{
		{"invalid mode", map[string]string{gateIsolationModeEnvVar: "bogus"},
			[]string{gateIsolationModeEnvVar, `"bogus"`, "auto, container, clone-sandbox, clone"}},
		{"invalid profile", map[string]string{deploymentProfileEnvVar: "cloud"},
			[]string{deploymentProfileEnvVar, `"cloud"`, "local, self-hosted, hosted"}},
		{"hosted+clone", map[string]string{deploymentProfileEnvVar: "hosted", gateIsolationModeEnvVar: "clone"},
			[]string{"hosted", "forbids", "clone"}},
		{"hosted+clone-sandbox", map[string]string{deploymentProfileEnvVar: "hosted", gateIsolationModeEnvVar: "clone-sandbox"},
			[]string{"hosted", "forbids", "clone-sandbox"}},
		{"defaults", nil, nil},
		{"hosted+container", map[string]string{deploymentProfileEnvVar: "hosted", gateIsolationModeEnvVar: "container", gateImageEnvVar: " img:1 "}, nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			st, err := configureGateIsolation(fakeEnv(r.env), gateiso.Probes{}, &log)
			if len(r.wantErr) > 0 {
				if err == nil {
					t.Fatalf("want error naming %v, got state %+v", r.wantErr, st)
				}
				for _, w := range r.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q does not name %q", err, w)
					}
				}
				if log.Len() != 0 {
					t.Errorf("a config error must not emit gate_isolation_configured:\n%s", log.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(log.String(), `"event":"gate_isolation_configured"`) {
				t.Errorf("missing gate_isolation_configured:\n%s", log.String())
			}
			if r.name == "defaults" && (st.mode != gateiso.ModeAuto || st.profile != gateiso.ProfileLocal || st.image != "") {
				t.Errorf("defaults = mode %q profile %q image %q, want auto/local/empty", st.mode, st.profile, st.image)
			}
			if r.name == "hosted+container" && (st.image != "img:1" || !strings.Contains(log.String(), `"image":"img:1"`)) {
				t.Errorf("image not trimmed/logged: %q\n%s", st.image, log.String())
			}
		})
	}
}

// TestRun_GateIsolationConfigErrorsExitUsage: run() rejects a misconfigured
// isolation env as a config error BEFORE any backend contact, and a valid
// configuration reaches exit 0 with the configured line in the log.
func TestRun_GateIsolationConfigErrorsExitUsage(t *testing.T) {
	args := []string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}
	rows := []struct {
		name string
		env  map[string]string
		want int
	}{
		{"bogus mode", map[string]string{gateIsolationModeEnvVar: "bogus"}, exitUsage},
		{"hosted+clone", map[string]string{deploymentProfileEnvVar: "hosted", gateIsolationModeEnvVar: "clone"}, exitUsage},
		{"valid", map[string]string{gateIsolationModeEnvVar: "clone"}, exitOK},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			for _, k := range []string{gateIsolationModeEnvVar, deploymentProfileEnvVar, gateImageEnvVar} {
				t.Setenv(k, r.env[k])
			}
			var out strings.Builder
			if got := run(args, &out); got != r.want {
				t.Fatalf("run = %d, want %d:\n%s", got, r.want, out.String())
			}
			if r.want == exitUsage {
				if !strings.Contains(out.String(), `"event":"runner_failed","reason":"config"`) {
					t.Errorf("missing runner_failed reason=config:\n%s", out.String())
				}
				if !strings.Contains(out.String(), `"event":"runner_started"`) {
					t.Errorf("the config check must run AFTER the startup line:\n%s", out.String())
				}
			} else if !strings.Contains(out.String(), `"event":"gate_isolation_configured","mode":"clone"`) {
				t.Errorf("missing gate_isolation_configured:\n%s", out.String())
			}
			if gateIsolation != nil {
				t.Errorf("run() must clear the process-wide state on exit")
			}
		})
	}
}

// TestGateIsolationState_HostedAutoUnsafeRefusedOnce: selection is computed
// once, logged once (carrying the endpoint), and refuses under hosted.
func TestGateIsolationState_HostedAutoUnsafeRefusedOnce(t *testing.T) {
	var log strings.Builder
	st := refusedState(&log)
	sel := st.selection(context.Background())
	sel2 := st.selection(context.Background())
	if !sel.Refused() || sel2 != sel {
		t.Fatalf("selection = %+v / %+v, want a stable refusal", sel, sel2)
	}
	if n := strings.Count(log.String(), `"event":"gate_isolation_selected"`); n != 1 {
		t.Errorf("gate_isolation_selected logged %d times, want 1:\n%s", n, log.String())
	}
	if !strings.Contains(log.String(), `"endpoint":{"raw":"tcp://10.0.0.5:2376"`) {
		t.Errorf("selected line must carry the runtime endpoint:\n%s", log.String())
	}
	if !strings.Contains(sel.Reason, "profile=hosted refuses") {
		t.Errorf("reason = %q", sel.Reason)
	}
}

// TestRunBoundedGateCommand_DefaultStateIsHostExec is the done-means pin: a
// NIL state (the shipped default for every direct caller) is the host exec.
func TestRunBoundedGateCommand_DefaultStateIsHostExec(t *testing.T) {
	installGateState(t, nil)
	if sel := gateIsolation.selection(context.Background()); sel.Path != gateiso.PathClone || sel.Reason != "unconfigured: host exec" {
		t.Fatalf("nil-state selection = %+v", sel)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	out, code := runBoundedGateCommand(context.Background(), "touch "+marker, dir, filepath.Join(dir, "lc"), time.Minute)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("host exec did not run the command: %v", err)
	}
}

// TestRunBoundedGateCommand_RefusedDoesNotExecute: a refused selection returns
// the signature and -1 and the command NEVER runs (deleting the refused branch
// of runBoundedGateArgv turns this red: the marker appears).
func TestRunBoundedGateCommand_RefusedDoesNotExecute(t *testing.T) {
	installGateState(t, refusedState(io.Discard))
	captureHostExec(t, true, 0)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	out, code, disp := runBoundedGateCommandDisposed(context.Background(), "touch "+marker, dir, filepath.Join(dir, "lc"), time.Minute)
	if code != -1 {
		t.Errorf("exit = %d, want -1", code)
	}
	if disp != gateRefused {
		t.Errorf("disposition = %s, want refused", disp)
	}
	if !strings.HasPrefix(out, gateIsolationRefusedSignature) {
		t.Errorf("output %q must lead with the refusal signature", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the gate command executed despite the refusal")
	}
}

// stubSeed swaps the container path's host-side seed for fn for the test.
func stubSeed(t *testing.T, fn func(checkout string) error) {
	t.Helper()
	prev := seedModCacheFn
	seedModCacheFn = func(_ context.Context, _ gateiso.SeedExecFunc, checkout, _ string, _ *gateiso.VisibleCaches, _ []string, _ time.Duration) (gateiso.SeedReport, error) {
		return gateiso.SeedReport{}, fn(checkout)
	}
	t.Cleanup(func() { seedModCacheFn = prev })
}

// stubBindEndpointEnv swaps the runtime CLI's endpoint binder for one that
// fails with err (approval condition 1 of #3448).
func stubBindEndpointEnv(t *testing.T, err error) {
	t.Helper()
	prev := bindEndpointEnvFn
	bindEndpointEnvFn = func(gateiso.Runtime, []string) ([]string, error) { return nil, err }
	t.Cleanup(func() { bindEndpointEnvFn = prev })
}

// shortTempDir is a SHORT checkout path for a row that plants a unix socket:
// socket paths are capped at ~104 bytes on macOS, and a t.TempDir under the
// long test-name directory exceeds it (bind: invalid argument → a silent SKIP
// that would leave the control unpinned).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fh-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestRunGateInContainer_PreExecFailures is the disposition table for every
// pre-exec branch of runGateInContainer (#3448): each row makes exactly ONE
// branch fail and asserts exit -1, the disposition, the output lead, that the
// runtime CLI was never reached, and that the output matches no infra
// signature (so the gates can never absorb it as a flake). The seed rows
// discriminate the classification decision — a HOST-caused seed failure is
// gateUnavailable (category C) while one wrapping gateiso.ErrSeedCheckout is
// gateCheckoutRefused (tree-attributable). Collapsing the two into one
// disposition in runGateInContainer turns the ErrSeedCheckout row red.
func TestRunGateInContainer_PreExecFailures(t *testing.T) {
	offline := errors.New("go mod download all: dial tcp: lookup proxy.golang.org: no such host")
	rows := []struct {
		name  string
		setup func(t *testing.T) (dir, lintCacheDir string)
		seed  func(checkout string) error // nil = the seed must NOT be reached
		bind  error                       // non-nil = the endpoint binder fails
		want  gateDisposition
		leads []string // every substring the output must carry
	}{
		{
			name: "visible-cache root creation fails",
			setup: func(t *testing.T) (string, string) {
				base := t.TempDir()
				// TMPDIR is read by os.MkdirTemp at call time; point it at a
				// directory that does not exist AFTER the row's own temp
				// paths are created.
				t.Setenv("TMPDIR", filepath.Join(base, "missing"))
				return base, filepath.Join(base, "lc")
			},
			want:  gateUnavailable,
			leads: []string{"gate container: gateiso: create visible cache root"},
		},
		{
			name: "lint-cache dir under a regular file",
			setup: func(t *testing.T) (string, string) {
				base := t.TempDir()
				file := filepath.Join(base, "file")
				if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return base, filepath.Join(file, "lc")
			},
			want:  gateUnavailable,
			leads: []string{"gate container: create lint cache dir:"},
		},
		{
			name: "mount guard refuses a unix socket in the checkout",
			setup: func(t *testing.T) (string, string) {
				dir := shortTempDir(t)
				sock := filepath.Join(dir, "nested", "s.sock")
				if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
					t.Fatal(err)
				}
				l, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatalf("unix socket unavailable: %v", err)
				}
				t.Cleanup(func() { _ = l.Close() })
				return dir, filepath.Join(t.TempDir(), "lc")
			},
			want:  gateUnavailable,
			leads: []string{"gate container:", "unix socket", "refused"},
		},
		{
			name:  "seed fails on the host (offline)",
			setup: func(t *testing.T) (string, string) { return t.TempDir(), filepath.Join(t.TempDir(), "lc") },
			seed:  func(string) error { return offline },
			want:  gateUnavailable,
			leads: []string{"gate container: seed module cache:", offline.Error()},
		},
		{
			name:  "seed refuses the checkout's own metadata",
			setup: func(t *testing.T) (string, string) { return t.TempDir(), filepath.Join(t.TempDir(), "lc") },
			seed: func(string) error {
				return fmt.Errorf("%w: go.sum is not a regular file", gateiso.ErrSeedCheckout)
			},
			want:  gateCheckoutRefused,
			leads: []string{"gate container: seed module cache:", "go.sum is not a regular file"},
		},
		{
			name:  "endpoint binding fails",
			setup: func(t *testing.T) (string, string) { return t.TempDir(), filepath.Join(t.TempDir(), "lc") },
			seed:  func(string) error { return nil },
			bind:  fmt.Errorf("%w: runtime %q has no validated socket path to bind", gateiso.ErrContainerSpec, gateiso.KindDocker),
			want:  gateUnavailable,
			leads: []string{"gate container:", "no validated socket path to bind"},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			dir, lc := r.setup(t)
			installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
			calls := captureHostExec(t, true, 0)
			var seeded []string
			stubSeed(t, func(checkout string) error {
				seeded = append(seeded, checkout)
				if r.seed == nil {
					t.Errorf("seed reached for a row whose failing branch precedes it (checkout %q)", checkout)
					return nil
				}
				return r.seed(checkout)
			})
			if r.bind != nil {
				stubBindEndpointEnv(t, r.bind)
			}
			out, code, disp := runBoundedGateCommandDisposed(context.Background(), "true", dir, lc, time.Minute)
			if code != -1 {
				t.Errorf("exit = %d, want -1", code)
			}
			if disp != r.want {
				t.Errorf("disposition = %s, want %s (out %q)", disp, r.want, out)
			}
			if !strings.HasPrefix(out, "gate container:") {
				t.Errorf("output %q must lead with the container pre-exec prefix", out)
			}
			for _, lead := range r.leads {
				if !strings.Contains(out, lead) {
					t.Errorf("output %q lacks %q", out, lead)
				}
			}
			if r.seed != nil && len(seeded) != 1 {
				t.Errorf("seed invoked %d times, want 1", len(seeded))
			}
			if len(*calls) != 0 {
				t.Errorf("runtime CLI reached with %q", *calls)
			}
			if isVerifyInfraFailure(out) {
				t.Errorf("pre-exec output must never match an infra signature (the gates would absorb it): %q", out)
			}
			if disp.neverExecutedInfra() != (r.want == gateUnavailable) {
				t.Errorf("neverExecutedInfra() = %t for %s", disp.neverExecutedInfra(), disp)
			}
		})
	}
}

// TestRunGateInContainer_MountGuardPrecedesSeed: the mount guard runs BEFORE
// the host-side seed, so a checkout the guard refuses (here: carrying a unix
// socket) is never handed to `go mod download` — the seed seam is not
// invoked at all, and the runtime CLI is not reached. Moving the seed back
// ahead of BuildArgv turns this red (the seed seam observes the checkout).
func TestRunGateInContainer_MountGuardPrecedesSeed(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fh-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("unix socket unavailable: %v", err)
	}
	defer l.Close()
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
	var seeded []string
	prev := seedModCacheFn
	seedModCacheFn = func(_ context.Context, _ gateiso.SeedExecFunc, checkout, _ string, _ *gateiso.VisibleCaches, _ []string, _ time.Duration) (gateiso.SeedReport, error) {
		seeded = append(seeded, checkout)
		return gateiso.SeedReport{}, nil
	}
	t.Cleanup(func() { seedModCacheFn = prev })
	calls := captureHostExec(t, true, 0)
	out, code := runBoundedGateCommand(context.Background(), "true", dir, filepath.Join(t.TempDir(), "lc"), time.Minute)
	if code != -1 || !strings.Contains(out, "unix socket") {
		t.Errorf("exit %d out %q; want -1 with the socket-mount refusal", code, out)
	}
	if len(seeded) != 0 {
		t.Errorf("seed ran against a checkout the mount guard refused: %q", seeded)
	}
	if len(*calls) != 0 {
		t.Errorf("runtime CLI reached with %q", *calls)
	}
}

// TestRunGateInContainer_ArgvAndTimeoutKill: the container path hands the
// runtime CLI a BuildArgv line (endpoint binding opens it, entrypoint reset
// precedes the image, the checkout is mounted at /work, GOPROXY=off crosses
// via -e) and a -1 from the exec triggers `rm -f <name>` under the same
// binding (deleting the KillArgv call turns this red).
func TestRunGateInContainer_ArgvAndTimeoutKill(t *testing.T) {
	dir := t.TempDir()
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
	calls := captureHostExec(t, false, -1)
	_, code := runBoundedGateCommand(context.Background(), "sleep 60", dir, filepath.Join(t.TempDir(), "lc"), time.Second)
	if code != -1 {
		t.Fatalf("exit = %d, want -1 propagated", code)
	}
	if len(*calls) != 2 {
		t.Fatalf("seam calls = %d, want run + rm -f: %q", len(*calls), *calls)
	}
	run := (*calls)[0]
	if strings.Join(run[:4], " ") != "docker --host unix:///nonexistent/daemon.sock run" {
		t.Errorf("argv[0..3] = %q, want docker --host unix:///nonexistent/daemon.sock run", run[:4])
	}
	joined := strings.Join(run, " ")
	for _, want := range []string{"--network=none", "--entrypoint  img:1 sh -c sleep 60", "-v " + dir + ":/work", "-e GOPROXY=off"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q lacks %q", joined, want)
		}
	}
	name := run[6]
	if kill := (*calls)[1]; strings.Join(kill, " ") != "docker --host unix:///nonexistent/daemon.sock rm -f "+name {
		t.Errorf("kill argv = %q, want docker --host unix:///nonexistent/daemon.sock rm -f %s", kill, name)
	}
}

// captureHostExecEnv is captureHostExec recording the ENV each seam call was
// handed alongside its argv.
func captureHostExecEnv(t *testing.T, code int) *[][2][]string {
	t.Helper()
	prev := execBoundedHostArgvFn
	var calls [][2][]string
	execBoundedHostArgvFn = func(_ context.Context, argv []string, _ string, env []string, _ time.Duration) (string, int) {
		calls = append(calls, [2][]string{append([]string(nil), argv...), append([]string(nil), env...)})
		return "captured", code
	}
	t.Cleanup(func() { execBoundedHostArgvFn = prev })
	return &calls
}

// TestRunGateInContainer_EndpointBoundAfterSelection pins the endpoint
// binding at the runner seam: the selection is recorded against socket S,
// THEN the runtime configuration changes (DOCKER_HOST → a remote tcp
// endpoint, DOCKER_CONTEXT → another context — the `docker context use`
// shape between two gates), and BOTH the run and the rm the runner launches
// still carry `--host unix://S` in argv and `DOCKER_HOST=unix://S` in env
// with the redirecting variables dropped. Deleting BindEndpointEnv in
// runGateInContainer (passing os.Environ()) turns the env half red; deleting
// EndpointArgs in BuildArgv/KillArgv turns the argv half red.
func TestRunGateInContainer_EndpointBoundAfterSelection(t *testing.T) {
	const sock = "/nonexistent/validated.sock"
	st := containerState("img:1", sock, io.Discard)
	installGateState(t, st)
	if sel := st.selection(context.Background()); sel.Path != gateiso.PathContainer || sel.Runtime.SocketPath != sock {
		t.Fatalf("selection = %+v", sel)
	}
	// Runtime configuration changes AFTER the validated selection.
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2376")
	t.Setenv("DOCKER_CONTEXT", "remote")
	t.Setenv("CONTAINER_HOST", "ssh://core@machine")
	t.Setenv("CONTAINER_CONNECTION", "machine")
	calls := captureHostExecEnv(t, -1)
	_, code := runBoundedGateCommand(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Second)
	if code != -1 || len(*calls) != 2 {
		t.Fatalf("exit %d, seam calls %d; want -1 with run + rm", code, len(*calls))
	}
	for i, what := range []string{"run", "rm"} {
		argv, env := (*calls)[i][0], (*calls)[i][1]
		if argv[0] != "docker" || argv[1] != "--host" || argv[2] != "unix://"+sock || argv[3] != what {
			t.Errorf("%s argv not bound to the validated socket: %q", what, argv[:4])
		}
		bound := false
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "DOCKER_HOST":
				if v != "unix://"+sock {
					t.Errorf("%s env DOCKER_HOST=%q, want unix://%s", what, v, sock)
				}
				bound = true
			case "DOCKER_CONTEXT", "CONTAINER_HOST", "CONTAINER_CONNECTION":
				t.Errorf("%s env carries redirecting variable %s", what, kv)
			}
		}
		if !bound {
			t.Errorf("%s env lacks DOCKER_HOST=unix://%s: %q", what, sock, env)
		}
	}
}

// TestRunGateInContainer_SeedUnderSanitizedEnvRefusesHostileMetadata pins
// the seed half of the container path at the runner seam: (1) the seed is
// handed the SANITIZED gate env — a runner credential set in the process
// environment is absent from it — never os.Environ(); (2) a checkout whose
// go.sum is a symlink to a host canary is REFUSED by the real
// gateiso.SeedModCache before the runtime CLI is reached, and the canary is
// untouched.
func TestRunGateInContainer_SeedUnderSanitizedEnvRefusesHostileMetadata(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	secret := "seed-canary-" + fmt.Sprint(os.Getpid())
	t.Setenv("FISHHAWK_GITHUB_TOKEN", secret)
	t.Setenv("ANTHROPIC_API_KEY", secret)
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))

	var seedEnv []string
	prev := seedModCacheFn
	seedModCacheFn = func(ctx context.Context, run gateiso.SeedExecFunc, checkout, host string, dest *gateiso.VisibleCaches, baseEnv []string, timeout time.Duration) (gateiso.SeedReport, error) {
		seedEnv = append([]string(nil), baseEnv...)
		return prev(ctx, run, checkout, host, dest, baseEnv, timeout)
	}
	t.Cleanup(func() { seedModCacheFn = prev })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canary, []byte("canary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, filepath.Join(dir, "go.sum")); err != nil {
		t.Fatal(err)
	}
	calls := captureHostExec(t, true, 0)
	out, code := runBoundedGateCommand(context.Background(), "true", dir, filepath.Join(t.TempDir(), "lc"), time.Minute)
	if code != -1 || !strings.Contains(out, "seed module cache") || !strings.Contains(out, "go.sum is not a regular file") {
		t.Errorf("exit %d out %q; want -1 with the ErrSeedCheckout refusal naming go.sum", code, out)
	}
	if len(*calls) != 0 {
		t.Errorf("runtime CLI reached despite the refused seed: %q", *calls)
	}
	if b, _ := os.ReadFile(canary); string(b) != "canary\n" {
		t.Errorf("canary changed: %q", b)
	}
	if seedEnv == nil {
		t.Fatal("seed never invoked")
	}
	for _, kv := range seedEnv {
		k, v, _ := strings.Cut(kv, "=")
		if v == secret || k == "FISHHAWK_GITHUB_TOKEN" || k == "ANTHROPIC_API_KEY" {
			t.Errorf("runner credential reached the seed env: %s", k)
		}
	}
	for _, want := range []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"} {
		if !containsString(seedEnv, want) {
			t.Errorf("seed env is not the sanitized gate env (lacks %s): %q", want, seedEnv)
		}
	}
}

// TestRunBoundedGateArgv_CloneSandboxWrapsArgv: the clone-sandbox path wraps
// the gate argv in unshare and passes it VERBATIM through the seam.
func TestRunBoundedGateArgv_CloneSandboxWrapsArgv(t *testing.T) {
	st, err := configureGateIsolation(fakeEnv(map[string]string{gateIsolationModeEnvVar: "clone-sandbox"}), gateiso.Probes{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return gateiso.Runtime{Kind: gateiso.KindNone} }
	st.probeSandbox = func(context.Context) (bool, string) { return true, "" }
	installGateState(t, st)
	calls := captureHostExec(t, false, 0)
	if _, code := runBoundedGateArgv(context.Background(), []string{"gofmt", "-l", "a b.go"}, t.TempDir(), t.TempDir(), time.Minute); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := (*calls)[0]
	if got[0] != "unshare" || got[len(got)-1] != "a b.go" || got[len(got)-3] != "gofmt" {
		t.Errorf("argv = %q, want unshare … gofmt -l 'a b.go'", got)
	}
}

// TestGateRefusalMessage_NotAnInfraFailure: the operator-facing refusal text
// must never be absorbed as an infra flake, and it leads with the signature.
func TestGateRefusalMessage_NotAnInfraFailure(t *testing.T) {
	msg := gateRefusalMessage(refusedState(io.Discard).selection(context.Background()))
	if isVerifyInfraFailure(msg) {
		t.Errorf("refusal must not match an infra signature: %q", msg)
	}
	if !strings.HasPrefix(msg, gateIsolationRefusedSignature) {
		t.Errorf("refusal text %q must lead with the signature", msg)
	}
}

// TestGateDisposition_String: every named disposition renders its label
// (the log lines and test failures read it), and an out-of-range value
// renders the numeric fallback rather than an empty string.
func TestGateDisposition_String(t *testing.T) {
	for _, tc := range []struct {
		d    gateDisposition
		want string
	}{
		{gateExecuted, "executed"},
		{gateCheckoutRefused, "checkout_refused"},
		{gateRefused, "refused"},
		{gateUnavailable, "unavailable"},
		{gateDisposition(99), "gateDisposition(99)"},
	} {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("gateDisposition(%d).String() = %q, want %q", int(tc.d), got, tc.want)
		}
	}
}

// TestRunBoundedGateArgvDisposed_EmptyArgvIsExecuted: an empty argv is a
// caller bug, not a deployment condition — ("", -1, gateExecuted), never a
// category-C disposition.
func TestRunBoundedGateArgvDisposed_EmptyArgvIsExecuted(t *testing.T) {
	installGateState(t, nil)
	out, code, disp := runBoundedGateArgvDisposed(context.Background(), nil, t.TempDir(), "", time.Second)
	if out != "" || code != -1 || disp != gateExecuted {
		t.Errorf("empty argv = (%q, %d, %s), want (\"\", -1, executed)", out, code, disp)
	}
}

// TestRunVerifyCommittedTree_SkipIsExecutedDisposition: the tolerant clone
// "skipped" branch (an unresolvable head) carries gateExecuted, so a gate
// site can never classify gate plumbing's own skip as category C.
func TestRunVerifyCommittedTree_SkipIsExecutedDisposition(t *testing.T) {
	installGateState(t, nil)
	repo, _ := gateRepoWithCommit(t)
	_, out, outcome, disp := runVerifyCommittedTree(context.Background(), "true", repo, "0000000000000000000000000000000000000000", time.Minute, nil)
	if outcome != "skipped" {
		t.Fatalf("outcome = %q out = %q, want the tolerant skipped", outcome, out)
	}
	if disp != gateExecuted || disp.neverExecutedInfra() {
		t.Errorf("skip disposition = %s, want executed (never category C)", disp)
	}
}

// gateRepoWithCommit returns a fixture repo whose in-scope edit is committed
// and the resulting head.
func gateRepoWithCommit(t *testing.T) (string, string) {
	t.Helper()
	repo, _, _ := verifiedTreeRepo(t)
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-m", "committed tree"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo, gitHead(t, repo)
}

// TestRunVerifyCommittedTree_RefusedIsFailedNeverSkipped: a refusal is a
// "failed" outcome carrying the signature — never the tolerant "skipped".
func TestRunVerifyCommittedTree_RefusedIsFailedNeverSkipped(t *testing.T) {
	installGateState(t, refusedState(io.Discard))
	repo, head := gateRepoWithCommit(t)
	_, out, outcome, disp := runVerifyCommittedTree(context.Background(), "true", repo, head, time.Minute, nil)
	if outcome != "failed" || disp != gateRefused || !strings.HasPrefix(out, gateIsolationRefusedSignature) {
		t.Errorf("outcome = %q disp = %s out = %q, want failed / refused with the refusal signature", outcome, disp, out)
	}
}

// TestRunVerifyCommittedTree_CloneIsIndependentAndCarriesPrimaryLock: the
// verify command runs in an independent CLONE (its common dir is its own,
// not the primary's) and sees FISHHAWK_VERIFY_LOCK_PATH naming the PRIMARY's
// lock (deleting the verifyLockPathEnv injection turns this red).
func TestRunVerifyCommittedTree_CloneIsIndependentAndCarriesPrimaryLock(t *testing.T) {
	installGateState(t, nil)
	repo, head := gateRepoWithCommit(t)
	primaryCommon, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := `printf 'common=%s\nlock=%s\n' "$(git rev-parse --path-format=absolute --git-common-dir)" "$FISHHAWK_VERIFY_LOCK_PATH"`
	_, out, outcome, _ := runVerifyCommittedTree(context.Background(), cmd, repo, head, time.Minute, nil)
	if outcome != "passed" {
		t.Fatalf("outcome %q: %s", outcome, out)
	}
	var common, lock string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "common="); ok {
			common = filepath.Clean(v) // the clone is swept on return, so it cannot be resolved here
		}
		if v, ok := strings.CutPrefix(l, "lock="); ok {
			lock = v
		}
	}
	if common == "" || common == primaryCommon || common == filepath.Join(repo, ".git") || !strings.Contains(common, "fishhawk-verify-") {
		t.Errorf("clone common dir = %q, want the throwaway clone's own .git, independent of the primary %q", common, primaryCommon)
	}
	if want := filepath.Join(repo, ".git", verifyLockFileName); lock != want {
		gotRes, _ := filepath.EvalSymlinks(filepath.Dir(lock))
		if lock == "" || gotRes != primaryCommon || filepath.Base(lock) != verifyLockFileName {
			t.Errorf("FISHHAWK_VERIFY_LOCK_PATH = %q, want the primary's %q", lock, want)
		}
	}
	// No worktree was registered against the primary.
	if wl, _ := exec.Command("git", "-C", repo, "worktree", "list").Output(); strings.Count(string(wl), "\n") != 1 {
		t.Errorf("a worktree is registered against the primary:\n%s", wl)
	}
}

// TestRunVerifyGateCommitted_RefusedIsCategoryC: the single-shot gate wraps a
// refusal in ErrVerifyInfraFailure (category C) + errGateIsolationRefused,
// runs verify exactly once and never fires the infra absorb.
func TestRunVerifyGateCommitted_RefusedIsCategoryC(t *testing.T) {
	installGateState(t, refusedState(io.Discard))
	repo, _, _ := verifiedTreeRepo(t)
	var log strings.Builder
	events, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), &log)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errGateIsolationRefused) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errGateIsolationRefused", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	if tree != "" {
		t.Errorf("a refused gate must not yield a verified tree")
	}
	runs, retries := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case "verify_run":
			runs++
		case "verify_infra_flake_retry":
			retries++
		}
	}
	if runs != 1 || retries != 0 {
		t.Errorf("verify_run = %d retries = %d, want 1 / 0", runs, retries)
	}
	if strings.Contains(log.String(), "verify_infra_flake_retry") {
		t.Errorf("absorb fired on a refusal:\n%s", log.String())
	}
}

// TestRunVerifyFixLoop_RefusedIsCategoryC: the fix loop breaks on a refusal
// with category C and NEVER re-invokes the fix agent.
func TestRunVerifyFixLoop_RefusedIsCategoryC(t *testing.T) {
	installGateState(t, refusedState(io.Discard))
	cfg, logPath := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	cfg.verifyMaxIterations = 2
	res := agent.Result{OK: true}
	var log strings.Builder
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &log)
	if err != nil || reinvoked || tree != "" {
		t.Fatalf("err=%v reinvoked=%t tree=%q", err, reinvoked, tree)
	}
	if res.OK || res.FailureCategory != "C" || !strings.HasPrefix(res.FailureReason, gateIsolationRefusedSignature) {
		t.Errorf("res = OK:%t cat:%q reason:%q, want category C with the signature", res.OK, res.FailureCategory, res.FailureReason)
	}
	if invoker.callIdx != 0 {
		t.Errorf("fix agent invoked %d times, want 0", invoker.callIdx)
	}
	if lines := readVerifyFormLog(t, logPath); len(lines) != 0 {
		t.Errorf("the verify command executed %d time(s) despite the refusal", len(lines))
	}
	for _, want := range []string{`"event":"verify_gate_refused"`, `"outcome":"failed"`} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "verify_infra_flake_retry") || strings.Contains(log.String(), "verify_fix_reinvoke") {
		t.Errorf("absorb or re-invoke fired on a refusal:\n%s", log.String())
	}
}

// unavailableContainerState installs a container-path state whose host-side
// seed fails for a HOST reason (an offline proxy lookup) — the
// gateUnavailable shape — and fails the test if the runtime CLI is reached.
func unavailableContainerState(t *testing.T) {
	t.Helper()
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
	captureHostExec(t, true, 0)
	stubSeed(t, func(string) error {
		return errors.New("go mod download all: dial tcp: lookup proxy.golang.org: no such host")
	})
}

// TestRunVerifyFixLoop_ContainerUnavailableIsCategoryC pins the #3448
// classification decision end to end through the fix loop: a host-caused
// container pre-exec failure (the seed cannot reach the proxy) is category C
// with verify_gate_unavailable logged, the fix agent NEVER invoked, and no
// absorb. Deleting the gateUnavailable case in runVerifyFixLoop turns this
// red: the failure falls through to the fix agent and demotes category A.
func TestRunVerifyFixLoop_ContainerUnavailableIsCategoryC(t *testing.T) {
	unavailableContainerState(t)
	cfg, logPath := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	cfg.verifyMaxIterations = 2
	res := agent.Result{OK: true}
	var log strings.Builder
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &log)
	if err != nil || reinvoked || tree != "" {
		t.Fatalf("err=%v reinvoked=%t tree=%q", err, reinvoked, tree)
	}
	if res.OK || res.FailureCategory != "C" || !strings.Contains(res.FailureReason, "seed module cache") {
		t.Errorf("res = OK:%t cat:%q reason:%q, want category C naming the seed failure", res.OK, res.FailureCategory, res.FailureReason)
	}
	if invoker.callIdx != 0 {
		t.Errorf("fix agent invoked %d times, want 0", invoker.callIdx)
	}
	if lines := readVerifyFormLog(t, logPath); len(lines) != 0 {
		t.Errorf("the verify command executed %d time(s) despite the unavailable container", len(lines))
	}
	for _, want := range []string{`"event":"verify_gate_unavailable"`, `"outcome":"failed"`} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, log.String())
		}
	}
	for _, never := range []string{"verify_infra_flake_retry", "verify_fix_reinvoke", "verify_gate_refused"} {
		if strings.Contains(log.String(), never) {
			t.Errorf("%s fired on an unavailable container:\n%s", never, log.String())
		}
	}
}

// TestRunVerifyGateCommitted_ContainerUnavailableIsCategoryC: the single-shot
// gate wraps a host-caused container pre-exec failure in
// ErrVerifyInfraFailure + errGateContainerUnavailable (category C), NOT
// errGateIsolationRefused, runs verify exactly once and never absorbs.
func TestRunVerifyGateCommitted_ContainerUnavailableIsCategoryC(t *testing.T) {
	unavailableContainerState(t)
	repo, _, _ := verifiedTreeRepo(t)
	var log strings.Builder
	events, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), &log)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errGateContainerUnavailable) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errGateContainerUnavailable", err)
	}
	if errors.Is(err, errGateIsolationRefused) {
		t.Errorf("an unavailable container must not read as a refusal: %v", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Errorf("committedGateFailureCategory = %q, want C", got)
	}
	if tree != "" {
		t.Errorf("an unavailable gate must not yield a verified tree")
	}
	runs, retries := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case "verify_run":
			runs++
		case "verify_infra_flake_retry":
			retries++
		}
	}
	if runs != 1 || retries != 0 {
		t.Errorf("verify_run = %d retries = %d, want 1 / 0", runs, retries)
	}
	if strings.Contains(log.String(), "verify_infra_flake_retry") {
		t.Errorf("absorb fired on an unavailable container:\n%s", log.String())
	}
}

// TestRunVerifyFixLoop_SeedCheckoutRefusedReachesFixAgent pins the other half
// of the #3448 decision: a seed failure wrapping gateiso.ErrSeedCheckout is
// the checkout's OWN metadata being refused — tree-attributable — so it stays
// on the fix-agent path (invoked once, category A on exhaustion, the reason
// naming the refused file) and fires no verify_gate_* event. Collapsing
// gateCheckoutRefused into gateUnavailable in runGateInContainer turns this
// red (category C, agent never invoked).
func TestRunVerifyFixLoop_SeedCheckoutRefusedReachesFixAgent(t *testing.T) {
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
	captureHostExec(t, true, 0)
	stubSeed(t, func(string) error {
		return fmt.Errorf("%w: go.sum is not a regular file", gateiso.ErrSeedCheckout)
	})
	cfg, _ := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	var log strings.Builder
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &log)
	if err != nil || !reinvoked || tree != "" {
		t.Fatalf("err=%v reinvoked=%t tree=%q, want a re-invoked loop", err, reinvoked, tree)
	}
	if res.OK || res.FailureCategory != "A" || !strings.Contains(res.FailureReason, "go.sum is not a regular file") {
		t.Errorf("res = OK:%t cat:%q reason:%q, want category A naming the refused file", res.OK, res.FailureCategory, res.FailureReason)
	}
	if invoker.callIdx != 1 {
		t.Errorf("fix agent invoked %d times, want 1", invoker.callIdx)
	}
	if !strings.Contains(log.String(), `"event":"verify_fix_reinvoke"`) {
		t.Errorf("log lacks verify_fix_reinvoke:\n%s", log.String())
	}
	if strings.Contains(log.String(), "verify_gate_refused") || strings.Contains(log.String(), "verify_gate_unavailable") {
		t.Errorf("a tree-attributable seed refusal must fire no verify_gate_* event:\n%s", log.String())
	}
}

// TestRunVerifyFixLoop_PrintedRefusalLiteralIsNotRefused is the
// counterfactual for #3448 note 2: a verify command that PRINTS the refusal
// literal as its first line and exits 1 is an ordinary red tree (category A,
// fix agent invoked, no verify_gate_refused), because classification reads
// the out-of-band disposition, never the untrusted output. Reinstating
// leading-prefix matching on the output in runVerifyFixLoop turns this red
// (category C, agent never invoked).
func TestRunVerifyFixLoop_PrintedRefusalLiteralIsNotRefused(t *testing.T) {
	installGateState(t, nil)
	cfg, _ := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	cfg.verifyCmd = fmt.Sprintf("printf '%%s planted by a test\\n' %q; exit 1", gateIsolationRefusedSignature)
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	var log strings.Builder
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &log)
	if err != nil || !reinvoked || tree != "" {
		t.Fatalf("err=%v reinvoked=%t tree=%q, want a re-invoked loop", err, reinvoked, tree)
	}
	if res.OK || res.FailureCategory != "A" {
		t.Errorf("res = OK:%t cat:%q reason:%q, want category A (a printed literal is not a refusal)", res.OK, res.FailureCategory, res.FailureReason)
	}
	if !strings.HasPrefix(strings.TrimSpace(res.FailureReason), "verify command") || !strings.Contains(res.FailureReason, gateIsolationRefusedSignature) {
		t.Errorf("reason %q should be the ordinary exhaustion reason carrying the printed output", res.FailureReason)
	}
	if invoker.callIdx != 1 {
		t.Errorf("fix agent invoked %d times, want 1", invoker.callIdx)
	}
	if strings.Contains(log.String(), "verify_gate_refused") {
		t.Errorf("a printed literal fired verify_gate_refused:\n%s", log.String())
	}
}

// ---------------------------------------------------------------------------
// Live container smoke + the TestMain sentinel (approval conditions 1 and 3).
// ---------------------------------------------------------------------------

// entrypointSmokeImage is the pinned e2e image (ENTRYPOINT git, so a missing
// `--entrypoint ”` makes `sh -c` fail with "'sh' is not a git command").
func entrypointSmokeImage() string {
	if v := os.Getenv("FISHHAWK_GATE_TEST_IMAGE"); v != "" {
		return v
	}
	return "docker.io/alpine/git:v2.47.2"
}

// independentRuntimeProbe is the sentinel's eligibility probe. It is
// deliberately INDEPENDENT of gateiso.DetectRuntime: a raw
// `docker version` / `podman version` exec plus an Lstat of a local socket
// candidate. A DetectRuntime regression that misclassifies a safe local
// runtime as UNSAFE makes the docker-gated fixtures skip while this probe
// still sees a runtime — and the sentinel FAILS the binary instead of
// letting the all-skipped run pass green.
func independentRuntimeProbe() bool {
	var cliOK bool
	for _, bin := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := exec.CommandContext(ctx, bin, "version").Run()
		cancel()
		if err == nil {
			cliOK = true
			break
		}
	}
	if !cliOK {
		return false
	}
	candidates := []string{"/var/run/docker.sock", "/run/docker.sock", "/run/podman/podman.sock"}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, ".docker", "run", "docker.sock"))
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "podman", "podman.sock"))
	}
	for _, raw := range []string{os.Getenv("DOCKER_HOST"), os.Getenv("CONTAINER_HOST")} {
		if p, ok := strings.CutPrefix(raw, "unix://"); ok {
			candidates = append(candidates, p)
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Lstat(c); err == nil && (st.Mode()&os.ModeSocket != 0 || st.Mode()&os.ModeSymlink != 0) {
			return true
		}
	}
	return false
}

// gateE2EEligible is the TestMain eligibility decision for the e2e sentinel:
// it returns probe() — the INDEPENDENT runtime probe — and NEVER consults
// detect (gateiso.DetectRuntime). The detect argument exists so the
// regression "eligibility gates on DetectRuntime.Safe" is pinnable: a
// DetectRuntime bug that misclassifies a safe local runtime as UNSAFE makes
// every docker-gated fixture skip, and the sentinel must still fire.
func gateE2EEligible(probe func() bool, _ func(context.Context, gateiso.Probes) gateiso.Runtime) bool {
	return probe()
}

// gateE2ESentinelShouldFire is the TestMain decision: a runtime is present
// (independent probe), the whole package ran (no -run filter, not -short) and
// no docker-gated fixture recorded a run.
func gateE2ESentinelShouldFire(eligible bool, ran int, runFilter string, short bool) bool {
	return eligible && runFilter == "" && !short && ran == 0
}

// gateE2ESentinelRunFilter reads the -test.run value TestMain gates on.
func gateE2ESentinelRunFilter() string {
	if f := flag.Lookup("test.run"); f != nil {
		return f.Value.String()
	}
	return ""
}

// TestGateE2ESentinel_FiresOnIndependentProbeNotDetectRuntime pins condition
// 3 of #2134 through the seam TestMain actually uses (gateE2EEligible, #3448
// note 4): with a probe that sees a runtime and a DetectRuntime stub that is
// UNSAFE (the regression that makes every docker-gated fixture skip), the
// sentinel is eligible and fires, and detect was consulted ZERO times. It
// runs on every host — no independentRuntimeProbe skip — because the probe
// is a stub. An implementation gating eligibility on detect's Safe verdict
// turns both the eligibility and the zero-call assertion red.
func TestGateE2ESentinel_FiresOnIndependentProbeNotDetectRuntime(t *testing.T) {
	detectCalls := 0
	detect := func(context.Context, gateiso.Probes) gateiso.Runtime {
		detectCalls++
		return unsafeRuntime()
	}
	eligible := gateE2EEligible(func() bool { return true }, detect)
	if !eligible {
		t.Errorf("eligible = false while the independent probe sees a runtime — eligibility must not read DetectRuntime")
	}
	if detectCalls != 0 {
		t.Errorf("DetectRuntime consulted %d time(s) for eligibility, want 0", detectCalls)
	}
	if !gateE2ESentinelShouldFire(eligible, 0, "", false) {
		t.Errorf("sentinel silent with eligible=%t and no fixture run", eligible)
	}
	if gateE2EEligible(func() bool { return false }, detect) || detectCalls != 0 {
		t.Errorf("a probe that sees no runtime must make eligibility false regardless of detect (calls=%d)", detectCalls)
	}
	if gateE2ESentinelShouldFire(true, 1, "", false) || gateE2ESentinelShouldFire(true, 0, "TestX", false) || gateE2ESentinelShouldFire(true, 0, "", true) || gateE2ESentinelShouldFire(false, 0, "", false) {
		t.Errorf("sentinel must be quiet when a fixture ran, under a -run filter, under -short, or without a runtime")
	}
}

// TestGateContainer_EntrypointResetSmoke is the live half of condition 1: the
// REAL container path (DetectRuntime with DefaultProbes, the real runtime CLI)
// executes `sh -c 'echo ok'` in the pinned image whose ENTRYPOINT is git.
// Deleting the `--entrypoint ”` pair in BuildArgv turns this red ("'sh' is
// not a git command"). It skips naming the reason when no safe runtime or
// the image is unavailable, and records a docker-gated run for the sentinel.
func TestGateContainer_EntrypointResetSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	rt := gateiso.DetectRuntime(context.Background(), gateiso.DefaultProbes())
	if !rt.Safe {
		t.Skipf("no safe container runtime: %s", rt.Reason)
	}
	image := entrypointSmokeImage()
	pull, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(pull, rt.Kind.Binary(), "pull", "-q", image).CombinedOutput(); err != nil {
		t.Skipf("image %s unavailable: %v: %s", image, err, out)
	}
	st, err := configureGateIsolation(fakeEnv(map[string]string{gateIsolationModeEnvVar: "container", gateImageEnvVar: image}), gateiso.DefaultProbes(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	installGateState(t, st)
	dir := t.TempDir()
	out, code := runBoundedGateCommand(context.Background(), "echo ok", dir, filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("container exec: exit %d output %q (mount sources may need a shared TMPDIR on Docker Desktop)", code, out)
	}
	dockerFixturesRan++
}

// gateSentinelReport renders the sentinel failure line TestMain prints.
func gateSentinelReport(ran int) string {
	return fmt.Sprintf("gate isolation e2e: runtime present but no docker-gated fixture ran (ran=%d)", ran)
}
