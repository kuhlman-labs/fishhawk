package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
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
	t.Cleanup(func() {
		gateIsolation = prev
		// The runner-owned docker config a container exec minted (#4046);
		// NOT st.cleanup(), whose cache-volume removal would reach the real
		// runtime once the test's seams are restored.
		if st != nil {
			st.removeDockerConfig()
		}
	})
}

// captureHostExec swaps the host-exec seam for a recorder. When fail is
// true, any call fails the test — the shape the refusal / mount-guard tests
// need to prove nothing executed.
func captureHostExec(t *testing.T, fail bool, code int) *[][]string {
	t.Helper()
	prev := execBoundedHostArgvFn
	var calls [][]string
	execBoundedHostArgvFn = func(_ context.Context, argv []string, _ string, _ []string, _ time.Duration) (string, int, bool) {
		calls = append(calls, append([]string(nil), argv...))
		if fail {
			t.Errorf("execBoundedHostArgvFn must not be reached; called with %q", argv)
		}
		return "captured", code, false
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
// runtime at sock and the given image: Select picks the container path. It
// pins FISHHAWK_GATE_CACHE=off so every pre-#3967 test keeps the per-exec
// caches and its exact runtime call sequence.
func containerState(image, sock string, logSink io.Writer) *gateIsolationState {
	return containerStateEnv(nil, image, sock, logSink, nil)
}

// containerStateEnv is containerState with extra env overriding the defaults
// (t may be nil when extra is nil).
func containerStateEnv(t *testing.T, image, sock string, logSink io.Writer, extra map[string]string) *gateIsolationState {
	env := map[string]string{gateIsolationModeEnvVar: "container", gateImageEnvVar: image, gateCacheEnvVar: "off"}
	for k, v := range extra {
		env[k] = v
	}
	st, err := configureGateIsolation(fakeEnv(env), gateiso.Probes{}, logSink)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return safeDockerRuntime(sock) }
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	return st
}

func TestConfigureGateIsolation_Rows(t *testing.T) {
	validDockerCfg, emptyDockerCfg := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(validDockerCfg, "config.json"), `{"credsStore":"desktop"}`)
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
		{"unknown gate service", map[string]string{gateServicesEnvVar: "postgres,redis"},
			[]string{gateServicesEnvVar, `"redis"`, "valid: postgres"}},
		{"defaults", nil, nil},
		{"services postgres", map[string]string{gateServicesEnvVar: " postgres , postgres ", gatePostgresImageEnvVar: " pg@sha256:abc "}, nil},
		{"hosted+container", map[string]string{deploymentProfileEnvVar: "hosted", gateIsolationModeEnvVar: "container", gateImageEnvVar: " img:1 "}, nil},
		// E51.3 / #2136: the allowlist and build posture are startup config.
		{"allowlist bare name", map[string]string{gateImageAllowlistEnvVar: "ghcr.io/org/, alpine"},
			[]string{gateImageAllowlistEnvVar, `"alpine"`, "ambiguous"}},
		{"allowlist host with tag-shaped port", map[string]string{gateImageAllowlistEnvVar: "x:tag"},
			[]string{gateImageAllowlistEnvVar, `"x:tag"`}},
		{"allowlist tag entry", map[string]string{gateImageAllowlistEnvVar: "ghcr.io/org/gate:main"},
			[]string{gateImageAllowlistEnvVar, `"ghcr.io/org/gate:main"`, "mutable"}},
		{"invalid build posture", map[string]string{gateBuildEnvVar: "maybe"},
			[]string{gateBuildEnvVar, `"maybe"`, "allow, deny"}},
		{"allowlist and build", map[string]string{gateImageAllowlistEnvVar: "ghcr.io/org/ docker.io/library/alpine", deploymentProfileEnvVar: "hosted", gateBuildEnvVar: "allow"}, nil},
		{"hosted build default", map[string]string{deploymentProfileEnvVar: "hosted"}, nil},
		// E51.18 / #3967: the cache posture is startup config.
		{"invalid cache mode", map[string]string{gateCacheEnvVar: "bogus"},
			[]string{gateCacheEnvVar, `"bogus"`, "process, off"}},
		{"cache off", map[string]string{gateCacheEnvVar: " off "}, nil},
		// E51.26 / #4046: the opt-in docker config is validated at startup.
		{"relative docker config", map[string]string{gateDockerConfigEnvVar: "rel/cfg"},
			[]string{gateDockerConfigEnvVar, `"rel/cfg" is not an absolute path`}},
		{"docker config without config.json", map[string]string{gateDockerConfigEnvVar: emptyDockerCfg},
			[]string{gateDockerConfigEnvVar, "no readable config.json"}},
		{"operator docker config", map[string]string{gateDockerConfigEnvVar: " " + validDockerCfg + " "}, nil},
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
			if r.name == "defaults" && (st.mode != gateiso.ModeAuto || st.profile != gateiso.ProfileLocal || st.image != "" ||
				len(st.services) != 0 || st.postgresImage != gateiso.DefaultPostgresImage || st.postgresServiceWanted()) {
				t.Errorf("defaults = mode %q profile %q image %q services %q pg %q, want auto/local/empty/none/%s",
					st.mode, st.profile, st.image, st.services, st.postgresImage, gateiso.DefaultPostgresImage)
			}
			if r.name == "services postgres" {
				if len(st.services) != 1 || !st.postgresServiceWanted() || st.postgresImage != "pg@sha256:abc" {
					t.Errorf("services = %q pg = %q, want [postgres] / pg@sha256:abc", st.services, st.postgresImage)
				}
				if !strings.Contains(log.String(), `"services":"postgres","postgres_image":"pg@sha256:abc"`) {
					t.Errorf("configured line lacks services/postgres_image:\n%s", log.String())
				}
			}
			if r.name == "hosted+container" && (st.image != "img:1" || !strings.Contains(log.String(), `"image":"img:1"`)) {
				t.Errorf("image not trimmed/logged: %q\n%s", st.image, log.String())
			}
			switch r.name {
			case "defaults":
				if !st.buildAllowed || !st.allowlist.Empty() || !strings.Contains(log.String(), `"allowlist_entries":0,"build":"allow"`) {
					t.Errorf("local default = build %t allowlist %v, want allow / empty:\n%s", st.buildAllowed, st.allowlist, log.String())
				}
			case "allowlist and build":
				if !st.buildAllowed || len(st.allowlist) != 2 || !strings.Contains(log.String(), `"allowlist_entries":2,"build":"allow"`) {
					t.Errorf("state = build %t allowlist %v:\n%s", st.buildAllowed, st.allowlist, log.String())
				}
			case "hosted build default":
				if st.buildAllowed || !strings.Contains(log.String(), `"build":"deny"`) {
					t.Errorf("hosted default build = %t, want deny:\n%s", st.buildAllowed, log.String())
				}
			case "cache off":
				if st.cacheMode != gateiso.CacheModeOff || !strings.Contains(log.String(), `"build":"allow","cache":"off"}`) {
					t.Errorf("cache = %q, want off (the LAST configured field):\n%s", st.cacheMode, log.String())
				}
			case "operator docker config":
				if st.credentials != gateiso.CredentialsOperatorConfig || st.dockerConfig == nil || st.dockerConfig.Dir != validDockerCfg ||
					st.dockerConfig.Owned() || !strings.Contains(log.String(), `"credentials":"operator_config"`) {
					t.Errorf("credentials = %q config = %+v, want operator_config at %s (not owned):\n%s", st.credentials, st.dockerConfig, validDockerCfg, log.String())
				}
			}
			// The shipped default (#4046): an UNSET FISHHAWK_GATE_DOCKER_CONFIG
			// is the anonymous posture, its config minted lazily (none yet).
			if r.name == "defaults" && (st.credentials != gateiso.CredentialsAnonymous || st.dockerConfig != nil ||
				!strings.Contains(log.String(), `"image":"","credentials":"anonymous"`)) {
				t.Errorf("default credentials = %q config = %+v, want anonymous / not yet minted:\n%s", st.credentials, st.dockerConfig, log.String())
			}
			// The shipped default (#3967): an UNSET FISHHAWK_GATE_CACHE is
			// process, logged as the configured line's LAST field.
			if r.name == "defaults" && (st.cacheMode != gateiso.CacheModeProcess || !strings.Contains(log.String(), `"build":"allow","cache":"process"}`)) {
				t.Errorf("default cache = %q, want process:\n%s", st.cacheMode, log.String())
			}
		})
	}
}

// TestRun_GateIsolationConfigErrorsExitUsage: run() rejects a misconfigured
// isolation env as a config error BEFORE any backend contact, and a valid
// configuration reaches exit 0 with the configured line in the log.
func TestRun_GateIsolationConfigErrorsExitUsage(t *testing.T) {
	validDockerCfg := t.TempDir()
	mustWrite(t, filepath.Join(validDockerCfg, "config.json"), `{"auths":{}}`)
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
		{"unknown gate service", map[string]string{gateServicesEnvVar: "redis"}, exitUsage},
		{"bad image allowlist", map[string]string{gateImageAllowlistEnvVar: "alpine"}, exitUsage},
		{"bad build posture", map[string]string{gateBuildEnvVar: "sometimes"}, exitUsage},
		{"bad cache mode", map[string]string{gateCacheEnvVar: "bogus"}, exitUsage},
		{"relative docker config", map[string]string{gateDockerConfigEnvVar: "rel/cfg"}, exitUsage},
		{"docker config without config.json", map[string]string{gateDockerConfigEnvVar: t.TempDir()}, exitUsage},
		{"valid", map[string]string{gateIsolationModeEnvVar: "clone"}, exitOK},
		{"valid operator docker config", map[string]string{gateIsolationModeEnvVar: "clone", gateDockerConfigEnvVar: validDockerCfg}, exitOK},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			for _, k := range []string{gateIsolationModeEnvVar, deploymentProfileEnvVar, gateImageEnvVar, gateServicesEnvVar, gatePostgresImageEnvVar, gateImageAllowlistEnvVar, gateBuildEnvVar, gateCacheEnvVar, gateDockerConfigEnvVar} {
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
				for _, k := range []string{gateServicesEnvVar, gateImageAllowlistEnvVar, gateBuildEnvVar, gateCacheEnvVar, gateDockerConfigEnvVar} {
					if r.env[k] != "" && !strings.Contains(out.String(), k) {
						t.Errorf("config error must name %s:\n%s", k, out.String())
					}
				}
				if strings.Contains(out.String(), `"event":"gate_isolation_configured"`) {
					t.Errorf("a config error must not configure:\n%s", out.String())
				}
			} else if !strings.Contains(out.String(), `"event":"gate_isolation_configured","mode":"clone"`) {
				t.Errorf("missing gate_isolation_configured:\n%s", out.String())
			} else {
				want := `"credentials":"anonymous"`
				if r.env[gateDockerConfigEnvVar] != "" {
					want = `"credentials":"operator_config"`
				}
				if !strings.Contains(out.String(), want) {
					t.Errorf("gate_isolation_configured lacks %s:\n%s", want, out.String())
				}
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
	bindEndpointEnvFn = func(gateiso.Runtime, []string, string) ([]string, error) { return nil, err }
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
		mint  error                       // non-nil = minting the runner docker config fails
		// state overrides the default container state (nil = containerState).
		state func(t *testing.T) *gateIsolationState
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
			// Binding precedes the passwd read and the seed: no runtime call
			// and no host-side go process runs under an unbound env.
			name:  "endpoint binding fails",
			setup: func(t *testing.T) (string, string) { return t.TempDir(), filepath.Join(t.TempDir(), "lc") },
			bind:  fmt.Errorf("%w: runtime %q has no validated socket path to bind", gateiso.ErrContainerSpec, gateiso.KindDocker),
			want:  gateUnavailable,
			leads: []string{"gate container:", "no validated socket path to bind"},
		},
		{
			// E51.26 / #4046: the runner-owned docker config precedes the
			// binding, so a mint failure leaves no runtime call to make.
			name:  "runner docker config cannot be minted",
			setup: func(t *testing.T) (string, string) { return t.TempDir(), filepath.Join(t.TempDir(), "lc") },
			mint:  errors.New("create runner docker config dir: read-only file system"),
			want:  gateUnavailable,
			leads: []string{"gate container: docker config:", "read-only file system"},
		},
		{
			// A blocked operator-config helper fails BEFORE any runtime call
			// that could pull — category C, never a hang.
			name: "operator credential helper blocked",
			setup: func(t *testing.T) (string, string) {
				credentialHelpersOnPath(t, map[string]string{"fhblock": "sleep 600"})
				shortCredentialProbe(t, 300*time.Millisecond)
				return t.TempDir(), filepath.Join(t.TempDir(), "lc")
			},
			state: func(t *testing.T) *gateIsolationState {
				return containerStateEnv(t, "img:1", "/nonexistent/daemon.sock", io.Discard,
					map[string]string{gateDockerConfigEnvVar: operatorDockerConfig(t, `{"credsStore":"fhblock"}`)})
			},
			want: gateUnavailable,
			leads: []string{"gate container: container_credentials_blocked: credential helper docker-credential-fhblock did not answer within 300ms for https://index.docker.io/v1/",
				"a locked keychain/screen"},
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			dir, lc := r.setup(t)
			st := containerState("img:1", "/nonexistent/daemon.sock", io.Discard)
			if r.state != nil {
				st = r.state(t)
			}
			installGateState(t, st)
			calls := captureHostExec(t, true, 0)
			aux := scriptAuxExec(t, nil)
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
			if r.mint != nil {
				prev := newAnonymousDockerConfigFn
				newAnonymousDockerConfigFn = func([]string) (gateiso.DockerConfig, error) { return gateiso.DockerConfig{}, r.mint }
				t.Cleanup(func() { newAnonymousDockerConfigFn = prev })
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
			if (r.bind != nil || r.mint != nil || r.state != nil) && len(aux.order) != 0 {
				t.Errorf("auxiliary runtime calls ran despite the pre-exec failure: %q", aux.order)
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
	aux := scriptAuxExec(t, nil)
	_, code := runBoundedGateCommand(context.Background(), "sleep 60", dir, filepath.Join(t.TempDir(), "lc"), time.Second)
	if code != -1 {
		t.Fatalf("exit = %d, want -1 propagated", code)
	}
	// #2137: every container exec reads the image passwd (and nothing else
	// auxiliary when no service is configured) …
	if strings.Join(aux.order, ",") != "passwd" {
		t.Errorf("auxiliary calls = %q, want exactly the passwd read", aux.order)
	}
	if len(*calls) != 2 {
		t.Fatalf("seam calls = %d, want run + rm -f: %q", len(*calls), *calls)
	}
	run := (*calls)[0]
	if strings.Join(run[:4], " ") != "docker --host unix:///nonexistent/daemon.sock run" {
		t.Errorf("argv[0..3] = %q, want docker --host unix:///nonexistent/daemon.sock run", run[:4])
	}
	joined := strings.Join(run, " ")
	for _, want := range []string{"--network=none", "--entrypoint  img:1 sh -c sleep 60", "-v " + dir + ":/work", "-e GOPROXY=off",
		// … and mounts it read-only with the gate-container marker pinned.
		":" + gateiso.MountPasswd + ":ro", "-e " + gateiso.GateContainerMarker} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q lacks %q", joined, want)
		}
	}
	if strings.Contains(joined, "FISHHAWK_TEST_PG_URL") || strings.Contains(joined, gateiso.MountPgSock) {
		t.Errorf("no service configured, yet the gate argv carries service wiring: %q", joined)
	}
	name := run[6]
	if kill := (*calls)[1]; strings.Join(kill, " ") != "docker --host unix:///nonexistent/daemon.sock rm -f "+name {
		t.Errorf("kill argv = %q, want docker --host unix:///nonexistent/daemon.sock rm -f %s", kill, name)
	}
}

// TestRunGateInContainer_TimedOutSeamIsTimedOutDisposition (#3383): when the
// host-exec seam reports the runner's own deadline expired, the container
// path maps it to gateTimedOut (never gateExecuted) AND still issues the
// `rm -f` KillArgv cleanup the -1 exit already triggers — the disposition
// relabels the outcome, it does not skip the teardown. The seam stub returns
// the partial `>> > ./runner` fragment so the mapping is proven to read the
// third value, never the output text.
func TestRunGateInContainer_TimedOutSeamIsTimedOutDisposition(t *testing.T) {
	dir := t.TempDir()
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
	prev := execBoundedHostArgvFn
	var calls [][]string
	execBoundedHostArgvFn = func(_ context.Context, argv []string, _ string, _ []string, _ time.Duration) (string, int, bool) {
		calls = append(calls, append([]string(nil), argv...))
		if len(calls) == 1 {
			return ">> > ./runner", -1, true
		}
		return "", 0, false
	}
	t.Cleanup(func() { execBoundedHostArgvFn = prev })
	out, code, disp := runBoundedGateCommandDisposed(context.Background(), "sleep 60", dir, filepath.Join(t.TempDir(), "lc"), time.Second)
	if disp != gateTimedOut {
		t.Fatalf("disposition = %s, want timed_out", disp)
	}
	if code != -1 || out != ">> > ./runner" {
		t.Errorf("(out, code) = (%q, %d), want the seam's partial fragment and -1", out, code)
	}
	if len(calls) != 2 {
		t.Fatalf("seam calls = %d, want run + rm -f: %q", len(calls), calls)
	}
	name := calls[0][6]
	if kill := calls[1]; strings.Join(kill, " ") != "docker --host unix:///nonexistent/daemon.sock rm -f "+name {
		t.Errorf("kill argv = %q, want docker --host unix:///nonexistent/daemon.sock rm -f %s", kill, name)
	}
}

// captureHostExecEnv is captureHostExec recording the ENV each seam call was
// handed alongside its argv.
func captureHostExecEnv(t *testing.T, code int) *[][2][]string {
	t.Helper()
	prev := execBoundedHostArgvFn
	var calls [][2][]string
	execBoundedHostArgvFn = func(_ context.Context, argv []string, _ string, env []string, _ time.Duration) (string, int, bool) {
		calls = append(calls, [2][]string{append([]string(nil), argv...), append([]string(nil), env...)})
		return "captured", code, false
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
		{gateTimedOut, "timed_out"},
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
	st, err := configureGateIsolation(fakeEnv(map[string]string{gateIsolationModeEnvVar: "container", gateImageEnvVar: image, gateCacheEnvVar: "off"}), gateiso.DefaultProbes(), io.Discard)
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

// ---------------------------------------------------------------------------
// Gate isolation evidence (#2135): the selection is RECORDED only when a gate
// reaches the exec seam (runBoundedGateArgvDisposed), and folded into
// gate_evidence from that record.
// ---------------------------------------------------------------------------

// fallbackState is a mode=auto/local state with no runtime and no image and
// no sandbox: Select falls back to clone.
func fallbackState() *gateIsolationState {
	st, err := configureGateIsolation(fakeEnv(nil), gateiso.Probes{}, io.Discard)
	if err != nil {
		panic(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime {
		return gateiso.Runtime{Kind: gateiso.KindNone, Reason: "no container runtime on PATH (docker or podman)"}
	}
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	return st
}

// TestGateIsolationEvidence_FallbackRecordedAtSeam drives the REAL seam: a gate
// exec on a fallback selection records it, and the evidence names the path,
// class and what the container path lacked.
func TestGateIsolationEvidence_FallbackRecordedAtSeam(t *testing.T) {
	installGateState(t, fallbackState())
	calls := captureHostExec(t, false, 0)
	if gateIsolationEvidenceFor(gateIsolation) != nil {
		t.Fatal("evidence recorded before any gate ran")
	}
	dir := t.TempDir()
	if _, code, disp := runBoundedGateArgvDisposed(context.Background(), []string{"true"}, dir, filepath.Join(dir, "lc"), time.Minute); code != 0 || disp != gateExecuted {
		t.Fatalf("exec = %d / %s", code, disp)
	}
	if len(*calls) != 1 {
		t.Fatalf("seam calls = %d, want 1", len(*calls))
	}
	iso := gateIsolationEvidenceFor(gateIsolation)
	if iso == nil {
		t.Fatal("a gate reached the seam but no selection was recorded")
	}
	if iso.Path != "clone" || iso.Class != "fallback" || iso.Mode != "auto" || iso.Profile != "local" || iso.RuntimeKind != "none" {
		t.Errorf("evidence = %+v", iso)
	}
	if !strings.Contains(iso.ContainerUnavailable, "no gate image configured (FISHHAWK_GATE_IMAGE is empty)") {
		t.Errorf("container_unavailable = %q, want it to name the empty image", iso.ContainerUnavailable)
	}
}

// TestGateIsolationEvidence_RefusedRecordedWithoutExec: a refused selection
// executes nothing yet IS recorded — a refusal is the most important record.
func TestGateIsolationEvidence_RefusedRecordedWithoutExec(t *testing.T) {
	installGateState(t, refusedState(io.Discard))
	captureHostExec(t, true, 0)
	dir := t.TempDir()
	if _, _, disp := runBoundedGateArgvDisposed(context.Background(), []string{"true"}, dir, filepath.Join(dir, "lc"), time.Minute); disp != gateRefused {
		t.Fatalf("disposition = %s, want refused", disp)
	}
	iso := gateIsolationEvidenceFor(gateIsolation)
	if iso == nil || iso.Path != "refused" || iso.Class != "refused" || iso.Profile != "hosted" {
		t.Fatalf("evidence = %+v, want a recorded refusal", iso)
	}
	if !strings.Contains(iso.ContainerUnavailable, "docker is not a safe runtime") {
		t.Errorf("container_unavailable = %q", iso.ContainerUnavailable)
	}
}

// TestGateIsolationEvidence_ContainerSelection: the container path through the
// seam records class container with no container_unavailable.
func TestGateIsolationEvidence_ContainerSelection(t *testing.T) {
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
	stubSeed(t, func(string) error { return nil })
	captureHostExec(t, false, 0)
	dir := t.TempDir()
	if _, code, disp := runBoundedGateArgvDisposed(context.Background(), []string{"true"}, dir, filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 || disp != gateExecuted {
		t.Fatalf("exec = %d / %s", code, disp)
	}
	iso := gateIsolationEvidenceFor(gateIsolation)
	if iso == nil || iso.Path != "container" || iso.Class != "container" || iso.Image != "img:1" || !iso.RuntimeSafe || iso.ContainerUnavailable != "" {
		t.Fatalf("evidence = %+v, want a recorded container selection", iso)
	}
}

// TestGateIsolationEvidence_ConfigurationCallerDoesNotRecord (approval
// condition 1): selection() has a NON-exec caller — runVerifyCommittedTree
// reads the path to decide its lock-path env — so deciding the selection must
// not record it; only the seam does. Setting the flag inside selection()'s
// once.Do turns this red.
func TestGateIsolationEvidence_ConfigurationCallerDoesNotRecord(t *testing.T) {
	installGateState(t, fallbackState())
	if sel := gateIsolation.selection(context.Background()); sel.Path != gateiso.PathClone {
		t.Fatalf("selection = %+v", sel)
	}
	if iso := gateIsolationEvidenceFor(gateIsolation); iso != nil {
		t.Fatalf("a configuration read of the selection recorded evidence: %+v", iso)
	}
}

// TestGateIsolationEvidence_NilStateAbsent: the unconfigured host exec (nil
// state) runs the gate but records nothing.
func TestGateIsolationEvidence_NilStateAbsent(t *testing.T) {
	installGateState(t, nil)
	captureHostExec(t, false, 0)
	dir := t.TempDir()
	_, _, _ = runBoundedGateArgvDisposed(context.Background(), []string{"true"}, dir, filepath.Join(dir, "lc"), time.Minute)
	if iso := gateIsolationEvidenceFor(gateIsolation); iso != nil {
		t.Fatalf("nil state recorded evidence: %+v", iso)
	}
}

// gateEvidencePayloadFromBundle returns the decoded gate_evidence payload of a
// packed bundle, failing when the bundle carries none.
func gateEvidencePayloadFromBundle(t *testing.T, data []byte) (gateEvidencePayload, string) {
	t.Helper()
	_, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == "gate_evidence" {
			var p gateEvidencePayload
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatalf("decode gate_evidence: %v", err)
			}
			return p, string(ev.Data)
		}
	}
	t.Fatalf("bundle carries no gate_evidence event")
	return gateEvidencePayload{}, ""
}

// TestGateIsolationEvidence_PlanStageEmitsNoMember (approval condition 1)
// drives the REAL stage path for a plan stage with a configured isolation
// state and a working-tree verify gate: the gate runs (so gate_evidence IS
// packed — the absence below is not vacuous) but never reaches the isolation
// seam, so the packed payload carries no gate_isolation member.
func TestGateIsolationEvidence_PlanStageEmitsNoMember(t *testing.T) {
	t.Setenv(gateIsolationModeEnvVar, "clone")
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	var stderr strings.Builder
	if got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "feature_change", "--stage", "plan",
		"--prompt-file", promptPath,
		"--bundle-out", bundlePath,
		"--verify-cmd", "true",
	}, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"gate_isolation_configured","mode":"clone"`) {
		t.Fatalf("isolation state was not configured:\n%s", stderr.String())
	}
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	p, raw := gateEvidencePayloadFromBundle(t, data)
	if len(p.VerifyRuns) != 1 {
		t.Fatalf("verify_runs = %d, want the working-tree gate's 1", len(p.VerifyRuns))
	}
	if p.GateIsolation != nil || strings.Contains(raw, "gate_isolation") {
		t.Errorf("a plan stage's gate_evidence carries gate_isolation:\n%s", raw)
	}
}

// TestGateIsolationEvidence_RefusedStageRawBundleCarriesMember (approval
// condition 2) drives run() for an implement stage whose committed-tree gate
// is REFUSED (mode=container with no image): the stage fails category C, still
// reaches the pre-pack region, and the RAW bundle shipped to the backend
// carries the refused gate_isolation member through the real pack path.
func TestGateIsolationEvidence_RefusedStageRawBundleCarriesMember(t *testing.T) {
	t.Setenv(gateIsolationModeEnvVar, "container")
	t.Setenv(gateImageEnvVar, "")
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}})
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "true",
		VerifyMaxIterations: 0,
		ScopeFiles:          []upload.ScopeFile{{Path: "mod/reg.go", Operation: "modify"}},
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("a refused gate must fail category C:\n%s", stderr.String())
	}
	var rawBundle []byte
	for _, c := range fu.gotShipCalls {
		if c.Variant == "raw" {
			rawBundle = c.Bundle
		}
	}
	if rawBundle == nil {
		t.Fatalf("no raw bundle shipped (calls=%d):\n%s", len(fu.gotShipCalls), stderr.String())
	}
	p, raw := gateEvidencePayloadFromBundle(t, rawBundle)
	if p.GateIsolation == nil {
		t.Fatalf("raw bundle gate_evidence carries no gate_isolation member:\n%s", raw)
	}
	if p.GateIsolation.Path != "refused" || p.GateIsolation.Class != "refused" || p.GateIsolation.Mode != "container" ||
		!strings.Contains(p.GateIsolation.ContainerUnavailable, "no gate image configured") {
		t.Errorf("gate_isolation = %+v, want the recorded mode=container refusal", p.GateIsolation)
	}
}

// ---------------------------------------------------------------------------
// Gate services and the caller passwd file (#2137): the container path reads
// the gate image's /etc/passwd and, with FISHHAWK_GATE_SERVICES=postgres,
// provisions a per-exec Postgres service through execGateAuxArgvFn; the gate
// itself still runs through execBoundedHostArgvFn.
// ---------------------------------------------------------------------------

// classifyGateArgv labels a recorded runtime command line (after the
// `<bin> --host unix://<sock>` binding) for order assertions.
func classifyGateArgv(argv []string) string {
	if len(argv) < 4 {
		return "?"
	}
	sub, joined := argv[3:], strings.Join(argv, " ")
	switch {
	case len(sub) > 1 && sub[0] == "volume" && sub[1] == "create" && strings.Contains(joined, gateiso.CacheVolumeLabel):
		return "cache_create"
	case len(sub) > 1 && sub[0] == "volume" && sub[1] == "rm" && strings.Contains(joined, "fishhawk-gate-cache-"):
		return "cache_rm"
	case len(sub) > 1 && sub[0] == "run" && sub[1] == "--rm" && (strings.Contains(joined, "mkdir -p -m 0700") || strings.Contains(joined, " install -d ")):
		return "cache_prepare"
	case len(sub) > 1 && sub[0] == "run" && sub[1] == "--rm" && strings.Contains(joined, ".fishhawk-write-probe"):
		return "cache_check"
	case strings.HasSuffix(joined, "cat /etc/passwd"):
		return "passwd"
	case len(sub) > 1 && sub[0] == "volume" && sub[1] == "create":
		return "volume_create"
	case len(sub) > 1 && sub[0] == "volume" && sub[1] == "rm":
		return "volume_rm"
	case len(sub) > 1 && sub[0] == "run" && sub[1] == "-d":
		return "service_run"
	case len(sub) > 1 && sub[0] == "run" && sub[1] == "--rm":
		return "gate_run"
	case sub[0] == "logs":
		return "logs"
	case sub[0] == "exec" && strings.Contains(joined, "pg_isready"):
		return "ready"
	case sub[0] == "exec" && strings.Contains(joined, " psql "):
		return "bootstrap"
	case len(sub) > 2 && sub[0] == "rm" && sub[1] == "-f" && sub[2] == "-v":
		return "service_rm"
	case len(sub) > 1 && sub[0] == "rm" && sub[1] == "-f":
		return "gate_kill"
	case len(sub) > 1 && sub[0] == "image" && sub[1] == "inspect":
		return "inspect"
	case sub[0] == "pull":
		return "pull"
	case sub[0] == "build":
		return "build"
	}
	return "?"
}

// gateExecScript records every runtime call on BOTH seams in one order and
// answers each through respond (label, 1-based per-label call count).
type gateExecScript struct {
	order   []string
	argv    map[string][]string // last argv per label
	env     map[string][]string // last env per label
	passwd  []byte              // content of the passwd file at gate_run time
	respond func(label string, n int) (string, int, bool)
	counts  map[string]int
}

func (g *gateExecScript) record(argv, env []string) (string, int, bool) {
	label := classifyGateArgv(argv)
	g.order = append(g.order, label)
	g.argv[label] = append([]string(nil), argv...)
	g.env[label] = append([]string(nil), env...)
	g.counts[label]++
	if label == "gate_run" {
		for i := 0; i+1 < len(argv); i++ {
			if src, ok := strings.CutSuffix(argv[i+1], ":"+gateiso.MountPasswd+":ro"); ok && argv[i] == "-v" {
				g.passwd, _ = os.ReadFile(src)
			}
		}
	}
	return g.respond(label, g.counts[label])
}

// serviceHappyPath answers every label as a healthy service would: the init
// line appears on the first log read, pg_isready and the bootstrap succeed,
// and the gate exits 0.
func serviceHappyPath(label string, _ int) (string, int, bool) {
	switch label {
	case "passwd":
		return "root:x:0:0:root:/root:/bin/sh\n", 0, false
	case "logs":
		return "server started\n" + gateiso.PostgresInitCompleteLine + "\n", 0, false
	case "gate_run":
		return "gate ok", 0, false
	}
	return "", 0, false
}

// scriptAuxExec swaps ONLY the auxiliary seam (passwd read + service
// lifecycle) for a recorder answering through respond (nil = the happy path).
func scriptAuxExec(t *testing.T, respond func(label string, n int) (string, int, bool)) *gateExecScript {
	t.Helper()
	if respond == nil {
		respond = serviceHappyPath
	}
	g := &gateExecScript{argv: map[string][]string{}, env: map[string][]string{}, counts: map[string]int{}, respond: respond}
	prev := execGateAuxArgvFn
	execGateAuxArgvFn = func(_ context.Context, argv []string, _ string, env []string, _ time.Duration) (string, int, bool) {
		return g.record(argv, env)
	}
	t.Cleanup(func() { execGateAuxArgvFn = prev })
	return g
}

// scriptGateExec swaps BOTH seams for one shared recorder, with the seed
// stubbed to succeed.
func scriptGateExec(t *testing.T, respond func(label string, n int) (string, int, bool)) *gateExecScript {
	t.Helper()
	g := scriptAuxExec(t, respond)
	prev := execBoundedHostArgvFn
	execBoundedHostArgvFn = func(_ context.Context, argv []string, _ string, env []string, _ time.Duration) (string, int, bool) {
		return g.record(argv, env)
	}
	t.Cleanup(func() { execBoundedHostArgvFn = prev })
	stubSeed(t, func(string) error { return nil })
	return g
}

// serviceContainerState is containerState with FISHHAWK_GATE_SERVICES=postgres
// (cache off like containerState; an extra may override it).
func serviceContainerState(t *testing.T, sock string, logSink io.Writer, extra map[string]string) *gateIsolationState {
	t.Helper()
	env := map[string]string{gateIsolationModeEnvVar: "container", gateImageEnvVar: "img:1", gateServicesEnvVar: "postgres", gateCacheEnvVar: "off"}
	for k, v := range extra {
		env[k] = v
	}
	st, err := configureGateIsolation(fakeEnv(env), gateiso.Probes{}, logSink)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return safeDockerRuntime(sock) }
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	installGateState(t, st)
	return st
}

// shortServiceReady shrinks the readiness poll for the test.
func shortServiceReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	prevT, prevI := gateServiceReadyTimeout, gateServiceReadyInterval
	gateServiceReadyTimeout, gateServiceReadyInterval = timeout, time.Millisecond
	t.Cleanup(func() { gateServiceReadyTimeout, gateServiceReadyInterval = prevT, prevI })
}

// observeService records gateServiceObserver calls (and the order position).
func observeService(t *testing.T, g *gateExecScript) *[]string {
	t.Helper()
	var seen []string
	prev := gateServiceObserver
	gateServiceObserver = func(_ context.Context, rt gateiso.Runtime, cliEnv []string, svc gateiso.PostgresService) {
		seen = append(seen, svc.Name)
		g.order = append(g.order, "observer")
		if rt.SocketPath == "" || len(cliEnv) == 0 {
			t.Errorf("observer handed an unbound runtime/env: %+v", rt)
		}
	}
	t.Cleanup(func() { gateServiceObserver = prev })
	return &seen
}

func argvHasPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// lastEnvValue returns the value the gate container sees for key: the LAST
// `-e key=…` in the argv wins in the runtime CLI.
func lastEnvValue(argv []string, key string) (string, bool) {
	val, ok := "", false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-e" {
			if v, found := strings.CutPrefix(argv[i+1], key+"="); found {
				val, ok = v, true
			}
		}
	}
	return val, ok
}

// TestRunGateInContainer_ServiceLifecycleOrder pins the provisioning order
// and the gate's service wiring: passwd read → volume create → run -d → logs
// → pg_isready → bootstrap → observer → gate run → rm -f -v → volume rm, every
// service argv naming the same fresh fishhawk-gate-svc-<hex>; the gate argv
// mounts that volume read-only at /pgsock and the passwd file read-only, pins
// FISHHAWK_GATE_CONTAINER=1, and carries the runner's DSN even when an extra
// tries to override it (service env LAST); the superuser password never
// reaches the gate argv or env.
func TestRunGateInContainer_ServiceLifecycleOrder(t *testing.T) {
	var log strings.Builder
	serviceContainerState(t, "/nonexistent/daemon.sock", &log, nil)
	g := scriptGateExec(t, nil)
	seen := observeService(t, g)
	out, code, disp := runBoundedGateArgvDisposed(context.Background(), []string{"sh", "-c", "true"}, t.TempDir(),
		filepath.Join(t.TempDir(), "lc"), time.Minute, "FISHHAWK_TEST_PG_URL=postgres://attacker@evil/x")
	if code != 0 || disp != gateExecuted || out != "gate ok" {
		t.Fatalf("(out, code, disp) = (%q, %d, %s), want the gate's own verdict", out, code, disp)
	}
	want := "passwd,volume_create,service_run,logs,ready,bootstrap,observer,gate_run,service_rm,volume_rm"
	if got := strings.Join(g.order, ","); got != want {
		t.Fatalf("call order:\n got %s\nwant %s", got, want)
	}
	name := g.argv["volume_create"][len(g.argv["volume_create"])-1]
	if !strings.HasPrefix(name, "fishhawk-gate-svc-") || len(*seen) != 1 || (*seen)[0] != name {
		t.Fatalf("service name %q / observer %q", name, *seen)
	}
	for _, l := range []string{"service_run", "logs", "ready", "bootstrap", "service_rm", "volume_rm"} {
		if !strings.Contains(strings.Join(g.argv[l], " "), name) {
			t.Errorf("%s argv does not name %s: %q", l, name, g.argv[l])
		}
	}
	gate := g.argv["gate_run"]
	if !argvHasPair(gate, "-v", name+":"+gateiso.MountPgSock+":ro") {
		t.Errorf("gate argv lacks the read-only service mount: %q", gate)
	}
	if v, _ := lastEnvValue(gate, "FISHHAWK_TEST_PG_URL"); v != gateiso.PostgresGateURL {
		t.Errorf("gate DSN = %q, want the runner's %q (an extra must not win)", v, gateiso.PostgresGateURL)
	}
	if v, _ := lastEnvValue(gate, "FISHHAWK_GATE_CONTAINER"); v != "1" {
		t.Errorf("gate argv lacks FISHHAWK_GATE_CONTAINER=1: %q", gate)
	}
	if !strings.Contains(strings.Join(gate, " "), ":"+gateiso.MountPasswd+":ro") {
		t.Errorf("gate argv lacks the passwd mount: %q", gate)
	}
	// The superuser password rides only in the bootstrap exec (-e PGPASSWORD).
	var super string
	for _, tok := range g.argv["bootstrap"] {
		if v, ok := strings.CutPrefix(tok, "PGPASSWORD="); ok {
			super = v
		}
	}
	if super == "" {
		t.Fatalf("bootstrap argv carries no PGPASSWORD: %q", g.argv["bootstrap"])
	}
	for _, tok := range append(append([]string(nil), gate...), g.env["gate_run"]...) {
		if strings.Contains(tok, super) {
			t.Errorf("the service superuser password reached the gate: %q", tok)
		}
	}
	if !strings.Contains(string(g.passwd), fmt.Sprintf(":x:%d:%d:fishhawk gate caller:", os.Getuid(), os.Getgid())) {
		t.Errorf("mounted passwd file lacks the caller entry:\n%s", g.passwd)
	}
	for _, w := range []string{`"event":"gate_service_provisioned","service":"` + name + `","volume":"` + name + `"`,
		`"event":"gate_service_removed","service":"` + name + `","volume":"` + name + `"`} {
		if !strings.Contains(log.String(), w) {
			t.Errorf("log lacks %s:\n%s", w, log.String())
		}
	}
}

// TestRunGateInContainer_ServiceProvisionFailures: one row per provisioning
// failure mode. Each is gateUnavailable (category C, never the fix agent)
// with -1 and a `provision postgres service: <step>` lead, the gate argv is
// NEVER executed, the observer never fires, and — once `volume create` was
// attempted — the teardown (rm -f -v, volume rm) still runs.
func TestRunGateInContainer_ServiceProvisionFailures(t *testing.T) {
	failOn := func(label string, out string) func(string, int) (string, int, bool) {
		return func(l string, n int) (string, int, bool) {
			if l == label {
				return out, 1, false
			}
			return serviceHappyPath(l, n)
		}
	}
	rows := []struct {
		name         string
		extra        map[string]string
		respond      func(string, int) (string, int, bool)
		lead         string
		contains     []string // further substrings the output must carry
		noReady      bool     // pg_isready must never be consulted
		wantTeardown bool
	}{
		{name: "service argv refused (flag-shaped image)", extra: map[string]string{gatePostgresImageEnvVar: "--privileged"},
			lead: "gate container: provision postgres service: argv:"},
		{name: "volume create fails", respond: failOn("volume_create", "no space left"),
			lead: "gate container: provision postgres service: volume create: exit 1: no space left", wantTeardown: true},
		{name: "service run fails", respond: failOn("service_run", "pull access denied"),
			lead: "gate container: provision postgres service: run: exit 1: pull access denied", wantTeardown: true},
		{name: "readiness times out (pg_isready never answers)", respond: failOn("ready", "no response"),
			lead:         "gate container: provision postgres service: readiness: not ready within",
			contains:     []string{"(init-complete line seen: true): last pg_isready: no response; last service logs: server started"},
			wantTeardown: true},
		{name: "ready but init-complete line missing", respond: func(l string, n int) (string, int, bool) {
			if l == "logs" {
				// The temporary init server is up and answers pg_isready,
				// but the image never printed the init-complete line.
				return "waiting for server to start.... done\nserver started\ninitdb: error: invalid locale\n", 0, false
			}
			return serviceHappyPath(l, n)
		}, lead: "gate container: provision postgres service: readiness: not ready within",
			// The service logs explain the failure; the teardown removes them.
			contains: []string{"(init-complete line seen: false): pg_isready not run; last service logs:", "initdb: error: invalid locale"},
			noReady:  true, wantTeardown: true},
		{name: "service logs unreadable (logs exits non-zero)", respond: failOn("logs", "Error response from daemon: No such container: x"),
			lead:     "gate container: provision postgres service: readiness: not ready within",
			contains: []string{"last service logs: logs exit 1: Error response from daemon: No such container: x"},
			noReady:  true, wantTeardown: true},
		{name: "bootstrap fails", respond: failOn("bootstrap", "permission denied"),
			lead: "gate container: provision postgres service: bootstrap: exit 1: permission denied", wantTeardown: true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			shortServiceReady(t, 50*time.Millisecond)
			serviceContainerState(t, "/nonexistent/daemon.sock", io.Discard, r.extra)
			g := scriptGateExec(t, r.respond)
			seen := observeService(t, g)
			out, code, disp := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
			if code != -1 || disp != gateUnavailable {
				t.Errorf("(code, disp) = (%d, %s), want (-1, unavailable)", code, disp)
			}
			if !strings.HasPrefix(out, r.lead) {
				t.Errorf("output %q does not lead with %q", out, r.lead)
			}
			for _, w := range r.contains {
				if !strings.Contains(out, w) {
					t.Errorf("output %q lacks %q", out, w)
				}
			}
			if r.noReady && g.counts["ready"] != 0 {
				t.Errorf("pg_isready consulted %d times without a readable init-complete line: %q", g.counts["ready"], g.order)
			}
			if g.counts["gate_run"] != 0 || len(*seen) != 0 {
				t.Errorf("the gate executed (or the observer fired) despite a provisioning failure: %q", g.order)
			}
			torn := g.counts["service_rm"] == 1 && g.counts["volume_rm"] == 1
			if torn != r.wantTeardown {
				t.Errorf("teardown ran = %t, want %t: %q", torn, r.wantTeardown, g.order)
			}
			if !r.wantTeardown && g.counts["volume_create"]+g.counts["service_run"] != 0 {
				t.Errorf("service lifecycle ran despite the argv refusal: %q", g.order)
			}
			if isVerifyInfraFailure(out) {
				t.Errorf("a provisioning failure must never match an infra signature: %q", out)
			}
		})
	}
}

// TestRunGateInContainer_ReadinessReadsLogsFirst pins approval condition 3's
// interleaving: the image's temporary init server answers pg_isready (here:
// ALWAYS exit 0) while the logs do not yet carry the init-complete line, so
// pg_isready must not even be consulted until a log read showed the line, and
// the service is ready only on the first pg_isready AFTER that. A second row
// has the line present but pg_isready failing once: ready on the retry.
func TestRunGateInContainer_ReadinessReadsLogsFirst(t *testing.T) {
	rows := []struct {
		name    string
		respond func(string, int) (string, int, bool)
		want    string
	}{
		{"init server answers before the line", func(l string, n int) (string, int, bool) {
			if l == "logs" && n < 3 {
				return "server started\n", 0, false
			}
			return serviceHappyPath(l, n)
		}, "logs,logs,logs,ready,bootstrap"},
		{"line seen, pg_isready fails once", func(l string, n int) (string, int, bool) {
			if l == "ready" && n == 1 {
				return "no response", 2, false
			}
			return serviceHappyPath(l, n)
		}, "logs,ready,logs,ready,bootstrap"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			shortServiceReady(t, time.Minute)
			serviceContainerState(t, "/nonexistent/daemon.sock", io.Discard, nil)
			g := scriptGateExec(t, r.respond)
			_, code, disp := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
			if code != 0 || disp != gateExecuted {
				t.Fatalf("(code, disp) = (%d, %s), want (0, executed): %q", code, disp, g.order)
			}
			var got []string
			for _, l := range g.order {
				if l == "logs" || l == "ready" || l == "bootstrap" {
					got = append(got, l)
				}
			}
			if strings.Join(got, ",") != r.want {
				t.Errorf("readiness sequence = %s, want %s", strings.Join(got, ","), r.want)
			}
		})
	}
}

// TestRunGateInContainer_ServiceTornDownOnGateFailureAndTimeout: a red gate
// (exit 1) and a gate the runner's deadline killed both tear the service
// down, and the teardown does not alter the gate's own disposition.
func TestRunGateInContainer_ServiceTornDownOnGateFailureAndTimeout(t *testing.T) {
	rows := []struct {
		name     string
		gateCode int
		timedOut bool
		want     gateDisposition
		order    string
	}{
		{"gate exits 1", 1, false, gateExecuted, "gate_run,service_rm,volume_rm"},
		{"gate timed out", -1, true, gateTimedOut, "gate_run,gate_kill,service_rm,volume_rm"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			serviceContainerState(t, "/nonexistent/daemon.sock", io.Discard, nil)
			g := scriptGateExec(t, func(l string, n int) (string, int, bool) {
				if l == "gate_run" {
					return "red", r.gateCode, r.timedOut
				}
				return serviceHappyPath(l, n)
			})
			_, code, disp := runBoundedGateCommandDisposed(context.Background(), "false", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
			if code != r.gateCode || disp != r.want {
				t.Errorf("(code, disp) = (%d, %s), want (%d, %s)", code, disp, r.gateCode, r.want)
			}
			i := len(g.order) - strings.Count(r.order, ",") - 1
			if i < 0 || strings.Join(g.order[i:], ",") != r.order {
				t.Errorf("tail of call order = %q, want %s", g.order, r.order)
			}
		})
	}
}

// recordSeamCtx wraps BOTH seams installed by scriptGateExec so each call also
// records ctx.Err() under its label (the scripted fakes ignore ctx).
func recordSeamCtx(t *testing.T, g *gateExecScript) map[string]error {
	t.Helper()
	ctxErr := map[string]error{}
	prevAux, prevHost := execGateAuxArgvFn, execBoundedHostArgvFn
	wrap := func(ctx context.Context, argv []string, env []string) (string, int, bool) {
		ctxErr[classifyGateArgv(argv)] = ctx.Err()
		return g.record(argv, env)
	}
	execGateAuxArgvFn = func(ctx context.Context, argv []string, _ string, env []string, _ time.Duration) (string, int, bool) {
		return wrap(ctx, argv, env)
	}
	execBoundedHostArgvFn = func(ctx context.Context, argv []string, _ string, env []string, _ time.Duration) (string, int, bool) {
		return wrap(ctx, argv, env)
	}
	t.Cleanup(func() { execGateAuxArgvFn, execBoundedHostArgvFn = prevAux, prevHost })
	return ctxErr
}

// TestRunGateInContainer_CancelledParentContextStillTearsDown: a parent
// context cancelled mid-gate (a runner shutdown) or during the readiness poll
// must not leak the service — the teardown runs both steps on a context the
// cancellation does not reach (context.WithoutCancel). The readiness row parks
// the poll on an hour-long interval, so only the select's ctx.Done() arm can
// end it inside the guard; it must then be gateUnavailable, never the gate.
func TestRunGateInContainer_CancelledParentContextStillTearsDown(t *testing.T) {
	assertTornDown := func(t *testing.T, g *gateExecScript, ctxErr map[string]error) {
		t.Helper()
		if g.counts["service_rm"] != 1 || g.counts["volume_rm"] != 1 {
			t.Fatalf("teardown not run after the parent cancellation: %q", g.order)
		}
		for _, l := range []string{"service_rm", "volume_rm"} {
			if err := ctxErr[l]; err != nil {
				t.Errorf("%s ran on a cancelled context (%v): the service would leak", l, err)
			}
		}
	}

	t.Run("cancelled mid-gate", func(t *testing.T) {
		serviceContainerState(t, "/nonexistent/daemon.sock", io.Discard, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := scriptGateExec(t, func(l string, n int) (string, int, bool) {
			if l == "gate_run" {
				cancel() // the runner shuts down while the gate runs
				return "killed", -1, false
			}
			return serviceHappyPath(l, n)
		})
		ctxErr := recordSeamCtx(t, g)
		if _, code, disp := runBoundedGateCommandDisposed(ctx, "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute); code != -1 || disp != gateExecuted {
			t.Errorf("(code, disp) = (%d, %s), want (-1, executed): a parent cancellation is not a runner timeout", code, disp)
		}
		if ctxErr["gate_run"] != nil {
			t.Fatalf("fixture: the gate ran on an already-cancelled context")
		}
		assertTornDown(t, g, ctxErr)
	})

	t.Run("cancelled during the readiness poll", func(t *testing.T) {
		shortServiceReady(t, time.Hour)
		gateServiceReadyInterval = time.Hour
		serviceContainerState(t, "/nonexistent/daemon.sock", io.Discard, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := scriptGateExec(t, func(l string, n int) (string, int, bool) {
			if l == "logs" && n == 1 {
				// Cancel once the poll is parked on its interval timer.
				time.AfterFunc(20*time.Millisecond, cancel)
				return "server started\n", 0, false
			}
			return serviceHappyPath(l, n)
		})
		ctxErr := recordSeamCtx(t, g)
		type result struct {
			out  string
			code int
			disp gateDisposition
		}
		done := make(chan result, 1)
		go func() {
			out, code, disp := runBoundedGateCommandDisposed(ctx, "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
			done <- result{out, code, disp}
		}()
		var r result
		select {
		case r = <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the readiness poll did not observe the parent cancellation (ctx.Done) within 30s")
		}
		if r.code != -1 || r.disp != gateUnavailable {
			t.Errorf("(code, disp) = (%d, %s), want (-1, unavailable)", r.code, r.disp)
		}
		if !strings.HasPrefix(r.out, "gate container: provision postgres service: readiness:") {
			t.Errorf("output %q does not lead with the readiness failure", r.out)
		}
		if g.counts["gate_run"] != 0 || g.counts["bootstrap"] != 0 {
			t.Errorf("provisioning continued past the cancellation: %q", g.order)
		}
		assertTornDown(t, g, ctxErr)
	})
}

// TestRunGateInContainer_TeardownFailureDoesNotChangeVerdict: a failing
// rm / volume rm is logged (naming the cleanup label) and the green gate
// stays green.
func TestRunGateInContainer_TeardownFailureDoesNotChangeVerdict(t *testing.T) {
	var log strings.Builder
	serviceContainerState(t, "/nonexistent/daemon.sock", &log, nil)
	g := scriptGateExec(t, func(l string, n int) (string, int, bool) {
		if l == "service_rm" || l == "volume_rm" {
			return "daemon gone", 1, false
		}
		return serviceHappyPath(l, n)
	})
	out, code, disp := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
	if code != 0 || disp != gateExecuted || out != "gate ok" {
		t.Errorf("(out, code, disp) = (%q, %d, %s), want the green gate unchanged", out, code, disp)
	}
	if g.counts["service_rm"] != 1 || g.counts["volume_rm"] != 1 {
		t.Errorf("both teardown steps must be attempted: %q", g.order)
	}
	for _, w := range []string{`"event":"gate_service_cleanup_failed"`, "rm: exit 1: daemon gone", "volume rm: exit 1: daemon gone", gateiso.ServiceLabel} {
		if !strings.Contains(log.String(), w) {
			t.Errorf("log lacks %q:\n%s", w, log.String())
		}
	}
	if strings.Contains(log.String(), "gate_service_removed") {
		t.Errorf("a failed teardown must not log gate_service_removed:\n%s", log.String())
	}
}

// TestRunGateInContainer_ServiceArgvEndpointBound mirrors
// TestRunGateInContainer_EndpointBoundAfterSelection for the auxiliary calls:
// after DOCKER_HOST / DOCKER_CONTEXT are redirected post-selection, the
// passwd read and every service argv still open with `--host unix://<S>` and
// run under an env pinning DOCKER_HOST=unix://<S> with the redirect dropped.
func TestRunGateInContainer_ServiceArgvEndpointBound(t *testing.T) {
	const sock = "/nonexistent/validated.sock"
	st := serviceContainerState(t, sock, io.Discard, nil)
	if sel := st.selection(context.Background()); sel.Path != gateiso.PathContainer {
		t.Fatalf("selection = %+v", sel)
	}
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2376")
	t.Setenv("DOCKER_CONTEXT", "remote")
	g := scriptGateExec(t, nil)
	if _, code, _ := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 {
		t.Fatalf("exit %d: %q", code, g.order)
	}
	for _, l := range []string{"passwd", "volume_create", "service_run", "logs", "ready", "bootstrap", "service_rm", "volume_rm"} {
		argv, env := g.argv[l], g.env[l]
		if len(argv) < 3 || argv[0] != "docker" || argv[1] != "--host" || argv[2] != "unix://"+sock {
			t.Errorf("%s argv not bound to the validated socket: %q", l, argv)
		}
		if !containsString(env, "DOCKER_HOST=unix://"+sock) {
			t.Errorf("%s env lacks DOCKER_HOST=unix://%s", l, sock)
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "DOCKER_CONTEXT=") || kv == "DOCKER_HOST=tcp://10.0.0.5:2376" {
				t.Errorf("%s env carries redirect %s", l, kv)
			}
		}
	}
}

// TestRunGateInContainer_PasswdReadFailureDegrades (approval condition 7): a
// failed image passwd read DEGRADES — no /etc/passwd mount, the gate still
// executes, gate_passwd_unavailable is logged — and is NOT cached: the next
// exec reads again and, on success, mounts the file; that success IS cached,
// so a third exec does not read again. The passwd read argv is the hardened
// helper (approval condition 8).
func TestRunGateInContainer_PasswdReadFailureDegrades(t *testing.T) {
	var log strings.Builder
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", &log))
	g := scriptGateExec(t, func(l string, n int) (string, int, bool) {
		if l == "passwd" && n == 1 {
			return "Unable to find image", 125, false
		}
		return serviceHappyPath(l, n)
	})
	exec := func() string {
		t.Helper()
		if _, code, disp := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 || disp != gateExecuted {
			t.Fatalf("(code, disp) = (%d, %s), want the gate executed", code, disp)
		}
		return strings.Join(g.argv["gate_run"], " ")
	}
	if first := exec(); strings.Contains(first, gateiso.MountPasswd) {
		t.Errorf("a failed passwd read must degrade to no passwd mount: %q", first)
	}
	if !strings.Contains(log.String(), `"event":"gate_passwd_unavailable","image":"img:1"`) || !strings.Contains(log.String(), "exit 125") {
		t.Errorf("log lacks gate_passwd_unavailable:\n%s", log.String())
	}
	if second := exec(); !strings.Contains(second, ":"+gateiso.MountPasswd+":ro") {
		t.Errorf("the failed read was cached: no passwd mount on the retry: %q", second)
	}
	_ = exec()
	if g.counts["passwd"] != 2 {
		t.Errorf("passwd reads = %d, want 2 (fail, succeed, then cached)", g.counts["passwd"])
	}
	read := strings.Join(g.argv["passwd"], " ")
	for _, w := range []string{"--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", fmt.Sprintf("--user %d:%d", os.Getuid(), os.Getgid())} {
		if !strings.Contains(read, w) {
			t.Errorf("passwd read argv lacks %q: %q", w, read)
		}
	}
}

// TestRunGateInContainer_PasswdReadDropsPullNoise: the passwd read's seam
// returns the runtime CLI's combined stdout and stderr, so a cold pull of the
// gate image puts its progress lines beside the file with exit 0. Only the
// passwd entries are cached and mounted; an output carrying NO entry is a
// failed read — degraded and not cached, so the next exec reads again.
func TestRunGateInContainer_PasswdReadDropsPullNoise(t *testing.T) {
	const pull = "Unable to find image 'img:1' locally\n1: Pulling from library/img\n" +
		"Digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n" +
		"Status: Downloaded newer image for img:1\n"
	var log strings.Builder
	st := containerState("img:1", "/nonexistent/daemon.sock", &log)
	installGateState(t, st)
	g := scriptGateExec(t, func(l string, n int) (string, int, bool) {
		if l == "passwd" {
			if n == 1 {
				return pull, 0, false // noise only: no entry at all
			}
			return pull + "root:x:0:0:root:/root:/bin/sh\n", 0, false
		}
		return serviceHappyPath(l, n)
	})
	exec := func() string {
		t.Helper()
		if _, code, disp := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 || disp != gateExecuted {
			t.Fatalf("(code, disp) = (%d, %s), want the gate executed", code, disp)
		}
		return strings.Join(g.argv["gate_run"], " ")
	}
	if first := exec(); strings.Contains(first, gateiso.MountPasswd) {
		t.Errorf("a noise-only read must degrade to no passwd mount: %q", first)
	}
	if !strings.Contains(log.String(), `"event":"gate_passwd_unavailable","image":"img:1"`) || !strings.Contains(log.String(), "no well-formed passwd entry") {
		t.Errorf("log lacks gate_passwd_unavailable naming the empty read:\n%s", log.String())
	}
	if second := exec(); !strings.Contains(second, ":"+gateiso.MountPasswd+":ro") {
		t.Fatalf("the noise-only read was cached: no passwd mount on the retry: %q", second)
	}
	want := fmt.Sprintf("root:x:0:0:root:/root:/bin/sh\n%s:x:%d:%d:fishhawk gate caller:/tmp:/bin/sh\n",
		passwdTestName(), os.Getuid(), os.Getgid())
	if string(g.passwd) != want {
		t.Errorf("mounted passwd file:\n%q\nwant only the entries:\n%q", g.passwd, want)
	}
	st.passwdMu.Lock()
	cached := string(st.passwdByImage["img:1"])
	st.passwdMu.Unlock()
	if cached != "root:x:0:0:root:/root:/bin/sh\n" {
		t.Errorf("cached image passwd = %q, want the entry without the pull noise", cached)
	}
	if g.counts["passwd"] != 2 {
		t.Errorf("passwd reads = %d, want 2 (noise-only, then a cached success)", g.counts["passwd"])
	}
}

// passwdTestName is the caller-entry name gatePasswdFile writes on this host.
func passwdTestName() string {
	got := string(gateiso.BuildPasswd(nil, os.Getuid(), os.Getgid(), gateCallerName()))
	name, _, _ := strings.Cut(got, ":")
	return name
}

// TestRunGateInContainer_PasswdWriteFailureDegrades: a passwd WRITE failure
// degrades exactly like a read failure (approval condition 7).
func TestRunGateInContainer_PasswdWriteFailureDegrades(t *testing.T) {
	var log strings.Builder
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", &log))
	g := scriptGateExec(t, nil)
	prev := writePasswdFileFn
	writePasswdFileFn = func(string, []byte) (string, error) {
		return "", errors.New("write passwd file: no space left on device")
	}
	t.Cleanup(func() { writePasswdFileFn = prev })
	if _, code, disp := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 || disp != gateExecuted {
		t.Fatalf("(code, disp) = (%d, %s), want the gate executed", code, disp)
	}
	if gate := strings.Join(g.argv["gate_run"], " "); strings.Contains(gate, gateiso.MountPasswd) {
		t.Errorf("a failed passwd write must degrade to no passwd mount: %q", gate)
	}
	if !strings.Contains(log.String(), `"event":"gate_passwd_unavailable"`) || !strings.Contains(log.String(), "no space left on device") {
		t.Errorf("log lacks gate_passwd_unavailable naming the write failure:\n%s", log.String())
	}
}

// TestRunGateInContainer_NoServicesNoServiceCalls: with FISHHAWK_GATE_SERVICES
// unset the only auxiliary call is the passwd read; no volume/service argv is
// issued and the gate carries no DSN or socket mount.
func TestRunGateInContainer_NoServicesNoServiceCalls(t *testing.T) {
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", io.Discard))
	g := scriptGateExec(t, nil)
	if _, code, _ := runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := strings.Join(g.order, ","); got != "passwd,gate_run" {
		t.Errorf("call order = %s, want passwd,gate_run", got)
	}
	if gate := strings.Join(g.argv["gate_run"], " "); strings.Contains(gate, "FISHHAWK_TEST_PG_URL") || strings.Contains(gate, gateiso.MountPgSock) {
		t.Errorf("gate argv carries service wiring with no service configured: %q", gate)
	}
}

// TestGateIsolationState_ServicesIgnoredOffContainerPath: services configured
// on a runner whose selection falls back to clone log gate_services_ignored
// ONCE and provision nothing.
func TestGateIsolationState_ServicesIgnoredOffContainerPath(t *testing.T) {
	var log strings.Builder
	st, err := configureGateIsolation(fakeEnv(map[string]string{gateServicesEnvVar: "postgres"}), gateiso.Probes{}, &log)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime {
		return gateiso.Runtime{Kind: gateiso.KindNone, Reason: "no container runtime"}
	}
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	installGateState(t, st)
	aux := scriptAuxExec(t, nil)
	for i := 0; i < 2; i++ {
		if _, code := runBoundedGateCommand(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 {
			t.Fatalf("clone exec %d: exit %d", i, code)
		}
	}
	if n := strings.Count(log.String(), `"event":"gate_services_ignored","services":"postgres","path":"clone"`); n != 1 {
		t.Errorf("gate_services_ignored logged %d times, want 1:\n%s", n, log.String())
	}
	if len(aux.order) != 0 {
		t.Errorf("auxiliary runtime calls off the container path: %q", aux.order)
	}
}

// TestGateOutputTail_Bounds: runtime CLI output carried into a provisioning
// error or a log line keeps only its trimmed TAIL, bounded.
func TestGateOutputTail_Bounds(t *testing.T) {
	if got := gateOutputTail("  short \n"); got != "short" {
		t.Errorf("short output = %q, want trimmed", got)
	}
	got := gateOutputTail(strings.Repeat("a", 3000) + "END")
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "END") || len(got) != len("…")+2048 {
		t.Errorf("long output not bounded to its tail: len %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Declared gate_container (E51.3 / #2136): the prompt's declaration reaches
// selection() through declare, the image policy runs there, and the container
// path resolves a declared image (explicit pull, run by digest) or an in-repo
// build (committed tree, static screen first, content-addressed cache) through
// the AUXILIARY seam before the gate's own run. The env image path is pinned
// byte-unchanged by TestRunGateInContainer_ArgvAndTimeoutKill above (exactly
// the passwd read on the aux seam, exactly run + rm -f on the gate seam).
// ---------------------------------------------------------------------------

const declSock = "/nonexistent/daemon.sock"

var (
	declDigestA = "sha256:" + strings.Repeat("a", 64)
	declDigestB = "sha256:" + strings.Repeat("b", 64)
	declDigestC = "sha256:" + strings.Repeat("c", 64)
	declImageID = "sha256:" + strings.Repeat("1", 64)
)

// declaredState configures a state from env, injects rt (no sandbox),
// declares gc and installs it.
func declaredState(t *testing.T, env map[string]string, rt gateiso.Runtime, gc *upload.GateContainerConfig, logSink io.Writer) *gateIsolationState {
	t.Helper()
	if logSink == nil {
		logSink = io.Discard
	}
	st, err := configureGateIsolation(fakeEnv(env), gateiso.Probes{}, logSink)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return rt }
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	st.declare(gc)
	installGateState(t, st)
	return st
}

func stageImage(ref string) *upload.GateContainerConfig {
	return &upload.GateContainerConfig{Image: ref, Source: gateiso.ImageSourceStage}
}

// gateRunImage is the image operand of a run argv (after `--entrypoint ""`).
func gateRunImage(argv []string) string {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--entrypoint" {
			return argv[i+2]
		}
	}
	return ""
}

// pulledImageResponder answers a declared-image resolution: the n-th inspect
// returns inspects[n-1] (exit 0) or "no such image" (exit 1) when absent or
// empty; pull exits pullCode; everything else is the service happy path.
func pulledImageResponder(pullCode int, inspects ...string) func(string, int) (string, int, bool) {
	return func(label string, n int) (string, int, bool) {
		switch label {
		case "inspect":
			if n <= len(inspects) && inspects[n-1] != "" {
				return inspects[n-1], 0, false
			}
			return "Error: No such image", 1, false
		case "pull":
			if pullCode != 0 {
				return "Error response from daemon: manifest unknown", pullCode, false
			}
			return "pulled", 0, false
		}
		return serviceHappyPath(label, n)
	}
}

func runDeclaredGate(t *testing.T, ctx context.Context, dir string) (string, int, gateDisposition) {
	t.Helper()
	return runBoundedGateArgvDisposed(ctx, []string{"true"}, dir, filepath.Join(t.TempDir(), "lc"), time.Minute)
}

// containerEnv is the declared-image tests' container-mode env, cache off
// (pre-#3967 call sequences) unless an extra overrides it.
func containerEnv(extra map[string]string) map[string]string {
	env := map[string]string{gateIsolationModeEnvVar: "container", gateCacheEnvVar: "off"}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// TestDeclaredImage_Resolution: a declared image beats FISHHAWK_GATE_IMAGE;
// a pinned ref present locally skips the pull, a pinned ref absent is pulled,
// a tag-only ref is ALWAYS pulled; the gate runs by name@digest every time and
// the resolved image is recorded. Dropping the declared branch from
// imageRequest turns every row red (the gate runs img:1, nothing resolves).
func TestDeclaredImage_Resolution(t *testing.T) {
	rows := []struct {
		name, ref  string
		inspects   []string
		wantOrder  string
		wantRun    string
		wantDigest string
	}{
		{"pinned present skips pull", "ghcr.io/o/g@" + declDigestA,
			[]string{declImageID + " ghcr.io/o/g@" + declDigestA},
			"inspect,passwd,gate_run", "ghcr.io/o/g@" + declDigestA, declDigestA},
		{"pinned with tag absent pulls", "ghcr.io/o/g:v1@" + declDigestA,
			[]string{"", declImageID + " ghcr.io/o/g@" + declDigestB + " ghcr.io/o/g@" + declDigestA},
			"inspect,pull,inspect,passwd,gate_run", "ghcr.io/o/g@" + declDigestA, declDigestA},
		{"tag-only always pulls and runs by digest", "ghcr.io/o/g:v1",
			[]string{declImageID + " ghcr.io/o/g@" + declDigestB},
			"pull,inspect,passwd,gate_run", "ghcr.io/o/g@" + declDigestB, declDigestB},
		{"docker hub familiar digest", "alpine:3.20",
			[]string{declImageID + " alpine@" + declDigestC},
			"pull,inspect,passwd,gate_run", "docker.io/library/alpine@" + declDigestC, declDigestC},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			st := declaredState(t, containerEnv(map[string]string{gateImageEnvVar: "img:1"}), safeDockerRuntime(declSock), stageImage(r.ref), &log)
			g := scriptGateExec(t, pulledImageResponder(0, r.inspects...))
			if _, code, disp := runDeclaredGate(t, context.Background(), t.TempDir()); code != 0 || disp != gateExecuted {
				t.Fatalf("exec = %d / %s", code, disp)
			}
			if got := strings.Join(g.order, ","); got != r.wantOrder {
				t.Errorf("runtime calls = %s, want %s", got, r.wantOrder)
			}
			if got := gateRunImage(g.argv["gate_run"]); got != r.wantRun {
				t.Errorf("gate ran image %q, want %q (never the env image img:1)", got, r.wantRun)
			}
			if got := gateRunImage(g.argv["passwd"]); got != r.wantRun {
				t.Errorf("passwd read image %q, want %q", got, r.wantRun)
			}
			sel, ok := st.recordedSelection()
			if !ok || sel.ImageSource != gateiso.ImageSourceStage || sel.ResolvedImage == nil ||
				sel.ResolvedImage.Ref != r.wantRun || sel.ResolvedImage.Digest != r.wantDigest || sel.ResolvedImage.ImageID != declImageID {
				t.Errorf("recorded = %+v / %+v", sel, sel.ResolvedImage)
			}
			if !strings.Contains(log.String(), `"event":"gate_container_resolved","source":"stage"`) {
				t.Errorf("missing gate_container_resolved:\n%s", log.String())
			}
		})
	}
}

// TestDeclaredImage_ResolutionFailuresAreUnavailable: every pull / inspect /
// digest failure is gateUnavailable (category C), the gate argv and the
// passwd read never run, and a pull failure names the local-only remedy
// (approval condition 7).
func TestDeclaredImage_ResolutionFailuresAreUnavailable(t *testing.T) {
	timedOutPull := func(label string, n int) (string, int, bool) {
		if label == "pull" {
			return "", -1, true
		}
		return pulledImageResponder(0)(label, n)
	}
	rows := []struct {
		name, ref string
		respond   func(string, int) (string, int, bool)
		want      []string
	}{
		{"tag-only local-only image cannot be pulled", "ghcr.io/o/local-only:dev", pulledImageResponder(1),
			[]string{"could not be pulled from its registry", "manifest unknown", "declare `dockerfile` + `context`", "FISHHAWK_GATE_IMAGE"}},
		{"pinned absent pull failure", "ghcr.io/o/g@" + declDigestA, pulledImageResponder(1),
			[]string{"could not be pulled", "exit 1"}},
		{"pull timeout", "ghcr.io/o/g:v1", timedOutPull,
			[]string{"could not be pulled", "timed out after 10m0s"}},
		{"tag-only without registry digest", "ghcr.io/o/g:v1", pulledImageResponder(0, declImageID),
			[]string{"has no registry digest", "local-only image"}},
		{"pinned present with unmatched digest", "ghcr.io/o/g@" + declDigestA, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestB),
			[]string{"do not include the pinned " + declDigestA}},
		{"inspect after pull fails", "ghcr.io/o/g@" + declDigestA, pulledImageResponder(0, "", ""),
			[]string{"after pull", "exit 1"}},
		{"inspect after pull unparsable", "ghcr.io/o/g:v1", pulledImageResponder(0, "garbage"),
			[]string{"after pull", "not a sha256 image id"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage(r.ref), nil)
			g := scriptGateExec(t, r.respond)
			out, code, disp := runDeclaredGate(t, context.Background(), t.TempDir())
			if code != -1 || disp != gateUnavailable {
				t.Fatalf("exec = %d / %s, want -1 / unavailable: %s", code, disp, out)
			}
			if !strings.HasPrefix(out, "gate container: gate_container unavailable: ") {
				t.Errorf("output %q lacks the unavailable lead", out)
			}
			for _, w := range r.want {
				if !strings.Contains(out, w) {
					t.Errorf("output %q does not name %q", out, w)
				}
			}
			if g.counts["gate_run"] != 0 || g.counts["passwd"] != 0 {
				t.Errorf("the gate (or passwd read) ran despite the resolution failure: %q", g.order)
			}
		})
	}
}

// TestDeclaredImage_PullUsesRunnerDockerConfigNotInherited (E51.26 / #4046,
// the inversion of the former inherited-DOCKER_CONFIG pin): the pull (and
// inspect) run under the runner's INHERITED environment bound to the
// validated socket — DOCKER_HOST re-pinned, DOCKER_CONTEXT dropped, HOME kept
// — but with the operator's DOCKER_CONFIG / REGISTRY_AUTH_FILE REPLACED by the
// runner-owned anonymous config (no credsStore, no credHelpers), and not
// under the sanitized gate env.
func TestDeclaredImage_PullUsesRunnerDockerConfigNotInherited(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2376")
	t.Setenv("DOCKER_CONTEXT", "remote")
	t.Setenv("DOCKER_CONFIG", "/runner/docker-config")
	t.Setenv("REGISTRY_AUTH_FILE", "/runner/auth.json")
	t.Setenv("FISHHAWK_TEST_INHERITED_ONLY", "1")
	st := declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/g:v1"), nil)
	g := scriptGateExec(t, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA))
	if _, code, _ := runDeclaredGate(t, context.Background(), t.TempDir()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if st.dockerConfig == nil || !st.dockerConfig.Owned() {
		t.Fatalf("no runner-owned docker config minted: %+v", st.dockerConfig)
	}
	minted := st.dockerConfig.Dir
	for _, label := range []string{"pull", "inspect"} {
		env := map[string]string{}
		for _, kv := range g.env[label] {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		if env["DOCKER_HOST"] != "unix://"+declSock {
			t.Errorf("%s DOCKER_HOST = %q, want the validated socket", label, env["DOCKER_HOST"])
		}
		if _, ok := env["DOCKER_CONTEXT"]; ok {
			t.Errorf("%s env carries DOCKER_CONTEXT", label)
		}
		if env["DOCKER_CONFIG"] != minted {
			t.Errorf("%s DOCKER_CONFIG = %q, want the runner-owned %q (never the inherited /runner/docker-config)", label, env["DOCKER_CONFIG"], minted)
		}
		if v, ok := env["REGISTRY_AUTH_FILE"]; ok {
			t.Errorf("%s env carries REGISTRY_AUTH_FILE=%q on docker", label, v)
		}
		if env["HOME"] == "" || env["FISHHAWK_TEST_INHERITED_ONLY"] != "1" {
			t.Errorf("%s env is not the inherited environment (HOME=%q, marker=%q)", label, env["HOME"], env["FISHHAWK_TEST_INHERITED_ONLY"])
		}
		if argv := g.argv[label]; argv[1] != "--host" || argv[2] != "unix://"+declSock {
			t.Errorf("%s argv not endpoint-bound: %q", label, argv)
		}
	}
	b, err := os.ReadFile(filepath.Join(minted, "config.json"))
	if err != nil || strings.Contains(string(b), "credsStore") || strings.Contains(string(b), "credHelpers") || !strings.Contains(string(b), `"auths":{"fishhawk.invalid":{}}`) {
		t.Errorf("runner-owned config.json = %s (%v), want only the credential-free placeholder auths entry and no store", b, err)
	}
	for _, kv := range sanitizedGateEnv() {
		if strings.HasPrefix(kv, "FISHHAWK_TEST_INHERITED_ONLY=") {
			t.Fatal("fixture invalid: the sanitized gate env keeps the marker, so the inherited-env assertion discriminates nothing")
		}
	}
}

// TestRunVerifyFixLoop_DeclaredPullFailureIsCategoryC: a declared image that
// cannot be pulled parks the fix loop category C — verify_gate_unavailable,
// the verify command never executed, the fix agent never invoked. Mapping
// the pull failure to gateExecuted turns this red (the agent is invoked).
func TestRunVerifyFixLoop_DeclaredPullFailureIsCategoryC(t *testing.T) {
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/local-only:dev"), nil)
	g := scriptGateExec(t, pulledImageResponder(1))
	cfg, logPath := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	cfg.verifyMaxIterations = 2
	res := agent.Result{OK: true}
	var log strings.Builder
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &log)
	if err != nil || reinvoked || tree != "" {
		t.Fatalf("err=%v reinvoked=%t tree=%q", err, reinvoked, tree)
	}
	if res.OK || res.FailureCategory != "C" || !strings.Contains(res.FailureReason, "could not be pulled") {
		t.Errorf("res = OK:%t cat:%q reason:%q, want category C naming the pull", res.OK, res.FailureCategory, res.FailureReason)
	}
	if invoker.callIdx != 0 {
		t.Errorf("fix agent invoked %d times, want 0", invoker.callIdx)
	}
	if lines := readVerifyFormLog(t, logPath); len(lines) != 0 || g.counts["gate_run"] != 0 {
		t.Errorf("the verify command executed despite the pull failure: %d log lines, %d gate runs", len(lines), g.counts["gate_run"])
	}
	if !strings.Contains(log.String(), `"event":"verify_gate_unavailable"`) {
		t.Errorf("log lacks verify_gate_unavailable:\n%s", log.String())
	}
}

// TestRunVerifyGateCommitted_DeclaredPullFailureIsCategoryC: the single-shot
// gate wraps it in ErrVerifyInfraFailure + errGateContainerUnavailable.
func TestRunVerifyGateCommitted_DeclaredPullFailureIsCategoryC(t *testing.T) {
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/g:v1"), nil)
	scriptGateExec(t, pulledImageResponder(1))
	repo, _, _ := verifiedTreeRepo(t)
	_, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), io.Discard)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errGateContainerUnavailable) || errors.Is(err, errGateIsolationRefused) {
		t.Fatalf("err = %v, want ErrVerifyInfraFailure + errGateContainerUnavailable", err)
	}
	if got := committedGateFailureCategory(err); got != "C" || tree != "" {
		t.Errorf("category = %q tree = %q, want C and no verified tree", got, tree)
	}
}

// TestHosted_DeclaredTagOnlyRefusedNeverFallsBackToEnv: under hosted a
// tag-only declared ref is a policy refusal; the gate is refused (nothing
// reaches the runtime) and the operator's env image is NEVER substituted.
func TestHosted_DeclaredTagOnlyRefusedNeverFallsBackToEnv(t *testing.T) {
	declaredState(t, containerEnv(map[string]string{deploymentProfileEnvVar: "hosted", gateImageEnvVar: "img:1", gateImageAllowlistEnvVar: "ghcr.io"}),
		safeDockerRuntime(declSock), stageImage("ghcr.io/o/g:v1"), nil)
	g := scriptGateExec(t, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA))
	out, code, disp := runDeclaredGate(t, context.Background(), t.TempDir())
	if code != -1 || disp != gateRefused {
		t.Fatalf("exec = %d / %s, want -1 / refused", code, disp)
	}
	if !strings.HasPrefix(out, gateIsolationRefusedSignature) || !strings.Contains(out, "not digest-pinned") || !strings.Contains(out, `image="ghcr.io/o/g:v1"`) {
		t.Errorf("refusal %q must name the declared tag-only ref", out)
	}
	if strings.Contains(out, "img:1") || len(g.order) != 0 {
		t.Errorf("the env image was substituted or the runtime reached: out=%q calls=%q", out, g.order)
	}
}

// TestHosted_DeclaredOffAllowlistRefusedCategoryC: an off-allowlist pinned
// ref is refused category C at the committed gate.
func TestHosted_DeclaredOffAllowlistRefusedCategoryC(t *testing.T) {
	declaredState(t, containerEnv(map[string]string{deploymentProfileEnvVar: "hosted", gateImageAllowlistEnvVar: "ghcr.io/org/"}),
		safeDockerRuntime(declSock), stageImage("ghcr.io/other/g@"+declDigestA), nil)
	g := scriptGateExec(t, nil)
	repo, _, _ := verifiedTreeRepo(t)
	_, _, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), io.Discard)
	if !errors.Is(err, errGateIsolationRefused) || committedGateFailureCategory(err) != "C" {
		t.Fatalf("err = %v, want a category C refusal", err)
	}
	if !strings.Contains(err.Error(), "not permitted by the operator image allowlist") || len(g.order) != 0 {
		t.Errorf("err = %v calls = %q", err, g.order)
	}
}

// TestHosted_DeclaredWithoutRuntimeRefused: hosted never runs a declared
// image on the host when no safe runtime exists.
func TestHosted_DeclaredWithoutRuntimeRefused(t *testing.T) {
	declaredState(t, map[string]string{deploymentProfileEnvVar: "hosted", gateImageAllowlistEnvVar: "ghcr.io"},
		unsafeRuntime(), stageImage("ghcr.io/o/g@"+declDigestA), nil)
	captureHostExec(t, true, 0)
	out, _, disp := runDeclaredGate(t, context.Background(), t.TempDir())
	if disp != gateRefused || !strings.Contains(out, "profile=hosted refuses") {
		t.Fatalf("disp = %s out = %q, want the hosted refusal", disp, out)
	}
	if sel, _ := gateIsolation.recordedSelection(); sel.DeclaredUnhonored != "" {
		t.Errorf("a refusal ran nothing on the host, yet it carries declared_unhonored %q", sel.DeclaredUnhonored)
	}
}

// TestLocal_DeclaredWithoutRuntimeRunsFallbackWithWarningAndMarker: local
// runs the fallback on the host, logs gate_container_unhonored (plus the
// tag-only policy warning) and records the declared_unhonored marker; an
// explicit clone mode marks "not attempted".
func TestLocal_DeclaredWithoutRuntimeRunsFallbackWithWarningAndMarker(t *testing.T) {
	rows := []struct {
		name, mode, want string
	}{
		{"auto without a safe runtime", "auto", "docker is not a safe runtime"},
		{"explicit clone mode", "clone", "not attempted: mode=clone"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			st := declaredState(t, map[string]string{gateIsolationModeEnvVar: r.mode}, unsafeRuntime(),
				&upload.GateContainerConfig{Image: "ghcr.io/o/g:v1", Source: gateiso.ImageSourceWorkflow}, &log)
			calls := captureHostExec(t, false, 0)
			if _, code, disp := runDeclaredGate(t, context.Background(), t.TempDir()); code != 0 || disp != gateExecuted {
				t.Fatalf("exec = %d / %s", code, disp)
			}
			if len(*calls) != 1 || strings.Join((*calls)[0], " ") != "true" {
				t.Errorf("host calls = %q, want the gate argv on the host", *calls)
			}
			sel, _ := st.recordedSelection()
			if sel.Path != gateiso.PathClone || !strings.Contains(sel.DeclaredUnhonored, "gate_container declared (workflow) but not honoured: ") ||
				!strings.Contains(sel.DeclaredUnhonored, r.want) {
				t.Errorf("selection = %s / %q", sel.Path, sel.DeclaredUnhonored)
			}
			for _, w := range []string{`"event":"gate_container_unhonored","source":"workflow","path":"clone"`, `"event":"gate_container_policy_warning","source":"workflow"`} {
				if !strings.Contains(log.String(), w) {
					t.Errorf("log lacks %s:\n%s", w, log.String())
				}
			}
		})
	}
}

// TestDeclare_AfterSelectionIgnored: a declaration after the selection was
// decided changes nothing and is logged; an empty or nil declaration is a
// no-op.
func TestDeclare_AfterSelectionIgnored(t *testing.T) {
	var log strings.Builder
	st := declaredState(t, containerEnv(map[string]string{gateImageEnvVar: "img:1"}), safeDockerRuntime(declSock), nil, &log)
	st.declare(&upload.GateContainerConfig{})
	if st.declared != nil || strings.Contains(log.String(), "gate_container_declared") {
		t.Fatalf("an empty declaration was recorded:\n%s", log.String())
	}
	sel := st.selection(context.Background())
	st.declare(stageImage("ghcr.io/o/g@" + declDigestA))
	if sel2 := st.selection(context.Background()); sel2.Image != "img:1" || sel2.ImageSource != gateiso.ImageSourceEnv || sel2 != sel {
		t.Errorf("selection after a late declaration = %+v, want the env selection unchanged", sel2)
	}
	if st.declared != nil || !strings.Contains(log.String(), `"event":"gate_container_declaration_ignored","source":"stage"`) {
		t.Errorf("late declaration not ignored with a log line:\n%s", log.String())
	}
	var nilState *gateIsolationState
	nilState.declare(stageImage("x")) // nil receiver: no panic
}

// TestDeclaredImage_EvidenceRecordsFinalDecidingGate (approval condition 4):
// two VERIFY gates resolve different digests and a later diff-coverage-style
// gate a third; the recorded image is the LAST verify gate's, with the
// distinct count; a later verify gate whose resolution fails records no image
// rather than inheriting an earlier one. Recording the most recent gate's
// image instead turns the first assertion red (digest C).
func TestDeclaredImage_EvidenceRecordsFinalDecidingGate(t *testing.T) {
	st := declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/g:v1"), nil)
	pullFails := false
	scriptGateExec(t, func(label string, n int) (string, int, bool) {
		if label == "pull" && pullFails {
			return "registry down", 1, false
		}
		digests := []string{declDigestA, declDigestB, declDigestC}
		if label == "inspect" && n <= len(digests) {
			return declImageID + " ghcr.io/o/g@" + digests[n-1], 0, false
		}
		return pulledImageResponder(0)(label, n)
	})
	verify := withDecidingGate(context.Background())
	for i, ctx := range []context.Context{verify, verify, context.Background()} {
		if _, code, _ := runDeclaredGate(t, ctx, t.TempDir()); code != 0 {
			t.Fatalf("gate %d exit %d", i, code)
		}
	}
	sel, _ := st.recordedSelection()
	if sel.ResolvedImage == nil || sel.ResolvedImage.Digest != declDigestB || sel.DistinctImagesCount != 3 {
		t.Fatalf("recorded = %+v count %d, want the last VERIFY gate's digest %s and 3 distinct", sel.ResolvedImage, sel.DistinctImagesCount, declDigestB)
	}
	pullFails = true
	if _, _, disp := runDeclaredGate(t, verify, t.TempDir()); disp != gateUnavailable {
		t.Fatalf("disposition = %s, want unavailable", disp)
	}
	if sel, _ := st.recordedSelection(); sel.ResolvedImage != nil || sel.DistinctImagesCount != 3 {
		t.Errorf("a failed deciding gate recorded %+v (count %d), want no image", sel.ResolvedImage, sel.DistinctImagesCount)
	}
}

// TestDeclaredImage_SingleImageHasNoDistinctCount: one image, no count.
func TestDeclaredImage_SingleImageHasNoDistinctCount(t *testing.T) {
	st := declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/g@"+declDigestA), nil)
	scriptGateExec(t, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA, declImageID+" ghcr.io/o/g@"+declDigestA))
	for i := 0; i < 2; i++ {
		if _, code, _ := runDeclaredGate(t, context.Background(), t.TempDir()); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	if sel, _ := st.recordedSelection(); sel.ResolvedImage == nil || sel.ResolvedImage.Digest != declDigestA || sel.DistinctImagesCount != 0 {
		t.Errorf("recorded = %+v count %d, want digest A and no count", sel.ResolvedImage, sel.DistinctImagesCount)
	}
}

// TestDiffCoverage_RunsInDeclaredImage: the diff-coverage measurement reaches
// the same seam, so it runs in the declared image (no diff_coverage rule).
func TestDiffCoverage_RunsInDeclaredImage(t *testing.T) {
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/g@"+declDigestA), nil)
	g := scriptGateExec(t, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA))
	repo, _ := gateRepoWithCommit(t)
	base := strings.TrimSpace(declGit(t, repo, "rev-parse", "HEAD~1"))
	ev := measureDiffCoverage(context.Background(), diffCoverageEvidence{BaseRef: "main"}, &upload.DiffCoverageConfig{Command: "make cover", ReportPath: "lcov.info"}, repo, base)
	if g.counts["gate_run"] != 1 {
		t.Fatalf("coverage command did not reach the container path (calls %q): %+v", g.order, ev)
	}
	if got := gateRunImage(g.argv["gate_run"]); got != "ghcr.io/o/g@"+declDigestA {
		t.Errorf("coverage ran in %q, want the declared image", got)
	}
	if !strings.Contains(strings.Join(g.argv["gate_run"], " "), "sh -c make cover") {
		t.Errorf("gate argv %q lacks the coverage command", g.argv["gate_run"])
	}
}

// TestAutoformat_RunsInDeclaredImage: the auto-format absorb reaches the same
// seam, so the formatter runs in the declared image.
func TestAutoformat_RunsInDeclaredImage(t *testing.T) {
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/g@"+declDigestA), nil)
	g := scriptGateExec(t, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA))
	repo := t.TempDir()
	mustWrite(t, filepath.Join(repo, "a.go"), "package a\n")
	if _, err := runAutoformat(context.Background(), repo, []string{"a.go"}, time.Minute); err != nil {
		t.Fatalf("runAutoformat: %v", err)
	}
	argv := g.argv["gate_run"]
	if got := gateRunImage(argv); got != "ghcr.io/o/g@"+declDigestA {
		t.Errorf("formatter ran in %q, want the declared image", got)
	}
	if !strings.HasSuffix(strings.Join(argv, " "), autoformatBinary+" fmt a.go") {
		t.Errorf("gate argv %q lacks the formatter argv", argv)
	}
}

func declGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// buildRepo is a git repository whose single commit holds files.
func buildRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	declGit(t, repo, "init", "--initial-branch=main")
	declGit(t, repo, "config", "user.name", "t")
	declGit(t, repo, "config", "user.email", "t@example.com")
	declGit(t, repo, "config", "commit.gpgsign", "false")
	for p, c := range files {
		full := filepath.Join(repo, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, full, c)
	}
	declGit(t, repo, "add", "-A")
	declGit(t, repo, "commit", "-m", "base")
	return repo
}

var declaredBuild = &upload.GateContainerConfig{Dockerfile: "gate/Dockerfile", Context: "gate", Source: gateiso.ImageSourceWorkflow}

// buildResponder scripts the build path: an inspect of a tag the script has
// built answers an id (else no such image); build records the tag as built
// and hands its argv to onBuild (nil = nothing) while the materialized
// context still exists; build exits buildCode.
func buildResponder(g **gateExecScript, buildCode int, onBuild func(argv []string)) func(string, int) (string, int, bool) {
	built := map[string]bool{}
	return func(label string, n int) (string, int, bool) {
		switch label {
		case "inspect":
			argv := (*g).argv["inspect"]
			if built[argv[len(argv)-1]] {
				return declImageID, 0, false
			}
			return "Error: No such image", 1, false
		case "build":
			argv := (*g).argv["build"]
			if onBuild != nil {
				onBuild(argv)
			}
			if buildCode != 0 {
				return "step 2/3 failed", buildCode, false
			}
			for i := 0; i+1 < len(argv); i++ {
				if argv[i] == "--tag" {
					built[argv[i+1]] = true
				}
			}
			return "built", 0, false
		}
		return serviceHappyPath(label, n)
	}
}

func flagValueOf(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

// TestDeclaredBuild_CacheMissBuildsCommittedTreeWithNetworkNone (approval
// condition 3): a cache miss builds with --network=none from the COMMITTED
// tree — a modified working-tree file contributes its committed bytes and an
// untracked file never enters the context — and the gate runs the
// content-addressed tag.
func TestDeclaredBuild_CacheMissBuildsCommittedTreeWithNetworkNone(t *testing.T) {
	repo := buildRepo(t, map[string]string{"gate/Dockerfile": "FROM alpine\nCOPY . /src\n", "gate/a.txt": "committed\n"})
	mustWrite(t, filepath.Join(repo, "gate", "a.txt"), "uncommitted edit\n")
	mustWrite(t, filepath.Join(repo, "gate", "untracked.txt"), "untracked\n")
	mustWrite(t, filepath.Join(repo, "gate", "Dockerfile"), "FROM evil/uncommitted\n")
	st := declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), declaredBuild, nil)
	var g *gateExecScript
	var seen string
	g = scriptGateExec(t, buildResponder(&g, 0, func(argv []string) {
		cx := argv[len(argv)-1]
		a, _ := os.ReadFile(filepath.Join(cx, "a.txt"))
		_, uerr := os.Stat(filepath.Join(cx, "untracked.txt"))
		df, _ := os.ReadFile(flagValueOf(argv, "--file"))
		seen = fmt.Sprintf("a=%q untracked_absent=%t dockerfile=%q", a, os.IsNotExist(uerr), df)
	}))
	if out, code, disp := runDeclaredGate(t, context.Background(), repo); code != 0 || disp != gateExecuted {
		t.Fatalf("exec = %d / %s: %s", code, disp, out)
	}
	if got := strings.Join(g.order, ","); got != "inspect,build,inspect,passwd,gate_run" {
		t.Errorf("runtime calls = %s", got)
	}
	if want := `a="committed\n" untracked_absent=true dockerfile="FROM alpine\nCOPY . /src\n"`; seen != want {
		t.Errorf("build context = %s, want %s", seen, want)
	}
	b := strings.Join(g.argv["build"], " ")
	tag := flagValueOf(g.argv["build"], "--tag")
	for _, w := range []string{"docker --host unix://" + declSock + " build --network=none --file ", "--label " + gateiso.GateBuildLabel} {
		if !strings.Contains(b, w) {
			t.Errorf("build argv %q lacks %q", b, w)
		}
	}
	if !strings.HasPrefix(tag, gateiso.GateBuildRepository+":") || gateRunImage(g.argv["gate_run"]) != tag {
		t.Errorf("gate ran %q, want the build tag %q", gateRunImage(g.argv["gate_run"]), tag)
	}
	sel, _ := st.recordedSelection()
	if r := sel.ResolvedImage; r == nil || r.Ref != tag || r.ContextDigest == "" || r.BuildDockerfile != "gate/Dockerfile" || r.BuildContext != "gate" || r.ImageID != declImageID || !sel.Build {
		t.Errorf("recorded = %+v / %+v", sel, sel.ResolvedImage)
	}
	if _, err := os.Stat(flagValueOf(g.argv["build"], "--file")); !os.IsNotExist(err) {
		t.Errorf("the materialized build dir was not removed: %v", err)
	}
}

// TestDeclaredBuild_TwoGateKindsShareDigestCacheHit (approval condition 3):
// the committed-tree verify gate (an independent clone) and the auto-format
// absorb (the dirty primary working tree) on ONE commit compute the same
// content digest, so the second is a cache hit and both run the same tag.
func TestDeclaredBuild_TwoGateKindsShareDigestCacheHit(t *testing.T) {
	repo := buildRepo(t, map[string]string{"gate/Dockerfile": "FROM alpine\n", "gate/a.txt": "committed\n", "a.go": "package a\n"})
	head := strings.TrimSpace(declGit(t, repo, "rev-parse", "HEAD"))
	mustWrite(t, filepath.Join(repo, "gate", "a.txt"), "dirty\n")
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), declaredBuild, nil)
	var g *gateExecScript
	g = scriptGateExec(t, buildResponder(&g, 0, nil))
	if _, _, outcome, disp := runVerifyCommittedTree(context.Background(), "true", repo, head, time.Minute, nil); outcome != "passed" || disp != gateExecuted {
		t.Fatalf("verify = %s / %s (calls %q)", outcome, disp, g.order)
	}
	first := gateRunImage(g.argv["gate_run"])
	if _, err := runAutoformat(context.Background(), repo, []string{"a.go"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if second := gateRunImage(g.argv["gate_run"]); second != first || !strings.HasPrefix(first, gateiso.GateBuildRepository+":") {
		t.Errorf("gate kinds ran %q then %q, want one content-addressed tag", first, second)
	}
	if g.counts["build"] != 1 {
		t.Errorf("builds = %d, want 1 (the second gate kind is a cache hit): %q", g.counts["build"], g.order)
	}
}

// TestDeclaredBuild_ModeOnlyChangeAltersDigest (approval condition 3): a
// commit that only flips a context file's mode moves the digest, so the tag
// changes and the image is rebuilt.
func TestDeclaredBuild_ModeOnlyChangeAltersDigest(t *testing.T) {
	repo := buildRepo(t, map[string]string{"gate/Dockerfile": "FROM alpine\n", "gate/run.sh": "echo hi\n"})
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), declaredBuild, nil)
	var g *gateExecScript
	g = scriptGateExec(t, buildResponder(&g, 0, nil))
	if _, code, _ := runDeclaredGate(t, context.Background(), repo); code != 0 {
		t.Fatal("first gate failed")
	}
	first := gateRunImage(g.argv["gate_run"])
	if err := os.Chmod(filepath.Join(repo, "gate", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	declGit(t, repo, "commit", "-am", "mode only")
	if _, code, _ := runDeclaredGate(t, context.Background(), repo); code != 0 {
		t.Fatal("second gate failed")
	}
	if second := gateRunImage(g.argv["gate_run"]); second == first || g.counts["build"] != 2 {
		t.Errorf("tags %q -> %q with %d builds, want a new digest and a rebuild", first, second, g.counts["build"])
	}
}

// TestDeclaredBuild_StaticRefusalBeforeBuild (approval conditions 1, 2, 6):
// every static refusal — a frontend-selecting directive in each form, each
// ADD source shape, the RUN network/security flags, a non-ASCII byte that
// splits a flag for the builder, an off-allowlist base in lower case (a
// numeric RUN --mount source included), and a cache mount under hosted — is
// gateRefused with the
// refusal named and NOTHING reaches the runtime (no inspect, no build, no
// gate). Skipping ScreenDockerfile turns every row red (a build call).
func TestDeclaredBuild_StaticRefusalBeforeBuild(t *testing.T) {
	pinnedBase := "ghcr.io/org/base@" + declDigestA
	hosted := map[string]string{deploymentProfileEnvVar: "hosted", gateImageAllowlistEnvVar: "ghcr.io/org/", gateBuildEnvVar: "allow"}
	allow := map[string]string{gateImageAllowlistEnvVar: "ghcr.io/org/"}
	rows := []struct {
		name, dockerfile, want string
		env                    map[string]string
	}{
		{"syntax directive", "# syntax=docker/dockerfile:1\nFROM alpine\n", "syntax", nil},
		{"c-style syntax directive", "// syntax=evil/frontend\nFROM alpine\n", "syntax", nil},
		{"json first line", "{\"syntax\": \"evil/frontend\"}\nFROM alpine\n", "first line is neither", nil},
		{"ADD url", "FROM alpine\nADD https://x/y /z\n", "ADD source", nil},
		{"lowercase add url", "FROM alpine\nadd http://x/y /z\n", "ADD source", nil},
		{"ADD variable", "FROM alpine\nARG U=https://x\nADD $U /x\n", "variable expansion", nil},
		{"ADD at sign", "FROM alpine\nADD a@b /src\n", "'@'", nil},
		{"ADD .git", "FROM alpine\nADD example/r.git /src\n", ".git", nil},
		{"ADD host-shaped", "FROM alpine\nADD github.com/o/r /src\n", "host-shaped", nil},
		{"ADD scp-like", "FROM alpine\nADD host:repo /x\n", "':'", nil},
		{"ADD not clean", "FROM alpine\nADD ../x /y\n", "'..'", nil},
		{"ADD vertical tab url", "FROM alpine\nADD\vhttps://host/x /y\n", "U+000B", nil},
		{"FROM form feed off-allowlist image", "FROM\fevil/base\n", "U+000C", allow},
		{"FROM bare CR off-allowlist image", "FROM\revil/base\n", "U+000D", allow},
		{"unknown first token", "FROM alpine\nADD\u200bhttps://x /y\n", "is not a Dockerfile instruction", nil},
		{"RUN mount quoted space", "FROM alpine\nRUN --mount=\"type=bind, from=evil/x,target=/x\" true\n", "quote or backslash", allow},
		{"RUN mount split on byte 0xA0", "FROM busybox\nRUN --mount=type=tmpfs,target=/t\xc3\xa0--mount=from=busybox,type=bind,target=/bb ls /bb\n", "U+00E0 on an instruction line is refused", nil},
		{"numeric mount source off-allowlist", "FROM ghcr.io/org/base AS a\nFROM ghcr.io/org/base\nRUN --mount=type=bind,from=0,target=/x cat /x/m\n", "RUN --mount from base docker.io/library/0 is not permitted", allow},
		{"mixed-case RUN network host", "FROM alpine\nRun --network=host true\n", "--network=host", nil},
		{"RUN security insecure", "FROM alpine\nRUN --security=insecure true\n", "--security=insecure", nil},
		{"lowercase from off-allowlist", "from evil/base\n", "not permitted by the operator image allowlist", allow},
		{"lowercase copy --from off-allowlist", "FROM ghcr.io/org/base\ncopy --from=evil/img /a /b\n", "not permitted by the operator image allowlist", allow},
		{"hosted unpinned base", "FROM ghcr.io/org/base:1\n", "not digest-pinned", hosted},
		{"hosted cache mount", "FROM " + pinnedBase + "\nRUN --mount=type=cache,target=/c true\n", "type=cache is refused under profile hosted", hosted},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			repo := buildRepo(t, map[string]string{"gate/Dockerfile": r.dockerfile, "gate/a.txt": "x\n"})
			declaredState(t, containerEnv(r.env), safeDockerRuntime(declSock), declaredBuild, nil)
			var g *gateExecScript
			g = scriptGateExec(t, buildResponder(&g, 0, nil))
			out, code, disp := runDeclaredGate(t, context.Background(), repo)
			if code != -1 || disp != gateRefused {
				t.Fatalf("exec = %d / %s, want -1 / refused: %s", code, disp, out)
			}
			if !strings.HasPrefix(out, gateIsolationRefusedSignature+" gate_container build refused: gate/Dockerfile at ") || !strings.Contains(out, r.want) {
				t.Errorf("refusal %q does not name %q", out, r.want)
			}
			if len(g.order) != 0 {
				t.Errorf("the runtime was reached before the static refusal: %q", g.order)
			}
		})
	}
}

// TestDeclaredBuild_LocalCacheMountAccepted (approval condition 6): outside
// hosted a cache mount is an accepted, documented limit — the build proceeds.
func TestDeclaredBuild_LocalCacheMountAccepted(t *testing.T) {
	repo := buildRepo(t, map[string]string{"gate/Dockerfile": "FROM alpine\nRUN --mount=type=cache,target=/c true\n"})
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), declaredBuild, nil)
	var g *gateExecScript
	g = scriptGateExec(t, buildResponder(&g, 0, nil))
	if out, code, disp := runDeclaredGate(t, context.Background(), repo); code != 0 || disp != gateExecuted || g.counts["build"] != 1 {
		t.Fatalf("exec = %d / %s builds %d: %s", code, disp, g.counts["build"], out)
	}
}

// TestDeclaredBuild_DisabledInHostedIsRefused: hosted denies in-repo builds
// by default (FISHHAWK_GATE_BUILD unset) even with an allowlist.
func TestDeclaredBuild_DisabledInHostedIsRefused(t *testing.T) {
	declaredState(t, containerEnv(map[string]string{deploymentProfileEnvVar: "hosted", gateImageAllowlistEnvVar: "ghcr.io/org/"}),
		safeDockerRuntime(declSock), declaredBuild, nil)
	g := scriptGateExec(t, nil)
	out, _, disp := runDeclaredGate(t, context.Background(), t.TempDir())
	if disp != gateRefused || !strings.Contains(out, "in-repo gate image builds are disabled under profile hosted (FISHHAWK_GATE_BUILD)") || len(g.order) != 0 {
		t.Fatalf("disp = %s out = %q calls = %q", disp, out, g.order)
	}
}

// TestDeclaredBuild_SourceAndBuildFailures: each failure after the policy
// decision maps to its disposition — an uncommitted Dockerfile is refused
// (tree-shaped, before any runtime call); a checkout git cannot read, an
// oversized context, a failed or timed-out build and a failed post-build
// inspect are gateUnavailable — and the gate never runs.
func TestDeclaredBuild_SourceAndBuildFailures(t *testing.T) {
	committed := map[string]string{"gate/Dockerfile": "FROM alpine\n", "gate/a.txt": "x\n", "gate/b.txt": "y\n"}
	rows := []struct {
		name      string
		repo      func(t *testing.T) string
		buildCode int
		timeout   bool
		noInspect bool
		limits    *gateiso.ContextLimits
		wantDisp  gateDisposition
		want      []string
	}{
		{name: "dockerfile not committed", repo: func(t *testing.T) string {
			r := buildRepo(t, map[string]string{"gate/a.txt": "x\n"})
			mustWrite(t, filepath.Join(r, "gate", "Dockerfile"), "FROM alpine\n")
			return r
		}, wantDisp: gateRefused, want: []string{"not present in the committed tree", "uncommitted files never enter a gate image"}},
		{name: "not a git checkout", repo: func(t *testing.T) string { return t.TempDir() }, wantDisp: gateUnavailable, want: []string{"resolve build source"}},
		{name: "symlinked context ignore file", repo: func(t *testing.T) string {
			r := buildRepo(t, committed)
			if err := os.Symlink("../a.go", filepath.Join(r, "gate", ".dockerignore")); err != nil {
				t.Fatal(err)
			}
			declGit(t, r, "add", "-A")
			declGit(t, r, "commit", "-m", "symlinked ignore file")
			return r
		}, wantDisp: gateRefused, want: []string{"materialize build context", "is a symlink"}},
		{name: "context too large", limits: &gateiso.ContextLimits{MaxEntries: 1, MaxBytes: 1 << 30}, wantDisp: gateUnavailable, want: []string{"too large", "narrower context"}},
		{name: "build fails", buildCode: 1, wantDisp: gateUnavailable, want: []string{"build of gate/Dockerfile failed: exit 1", "step 2/3 failed"}},
		{name: "build times out", timeout: true, wantDisp: gateUnavailable, want: []string{"build of gate/Dockerfile timed out after 20m0s"}},
		{name: "inspect after build fails", noInspect: true, wantDisp: gateUnavailable, want: []string{"after build"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			repo := ""
			if r.repo != nil {
				repo = r.repo(t)
			} else {
				repo = buildRepo(t, committed)
			}
			if r.limits != nil {
				prev := gateBuildContextLimits
				gateBuildContextLimits = *r.limits
				t.Cleanup(func() { gateBuildContextLimits = prev })
			}
			declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), declaredBuild, nil)
			var g *gateExecScript
			inner := buildResponder(&g, r.buildCode, nil)
			g = scriptGateExec(t, func(label string, n int) (string, int, bool) {
				if label == "build" && r.timeout {
					return "", -1, true
				}
				if label == "inspect" && r.noInspect {
					return "Error: No such image", 1, false
				}
				return inner(label, n)
			})
			out, code, disp := runDeclaredGate(t, context.Background(), repo)
			if code != -1 || disp != r.wantDisp {
				t.Fatalf("exec = %d / %s, want -1 / %s: %s", code, disp, r.wantDisp, out)
			}
			for _, w := range r.want {
				if !strings.Contains(out, w) {
					t.Errorf("output %q does not name %q", out, w)
				}
			}
			if g.counts["gate_run"] != 0 {
				t.Errorf("the gate ran despite the failure: %q", g.order)
			}
			if r.wantDisp == gateRefused && g.counts["build"] != 0 {
				t.Errorf("a refused source reached a build: %q", g.order)
			}
			if r.name == "dockerfile not committed" && len(g.order) != 0 {
				t.Errorf("an uncommitted Dockerfile reached the runtime: %q", g.order)
			}
		})
	}
}

// TestRunVerifyCommittedTree_IsTheDecidingGate: the committed-tree verify
// marks its gate as deciding, so a later auto-format absorb resolving a
// different digest does not displace the verify's image in the evidence.
// Deleting withDecidingGate at runVerifyCommittedTree's seam call turns this
// red (the absorb's digest B is recorded).
func TestRunVerifyCommittedTree_IsTheDecidingGate(t *testing.T) {
	st := declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), stageImage("ghcr.io/o/g:v1"), nil)
	scriptGateExec(t, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA, declImageID+" ghcr.io/o/g@"+declDigestB))
	repo, head := gateRepoWithCommit(t)
	if _, out, outcome, _ := runVerifyCommittedTree(context.Background(), "true", repo, head, time.Minute, nil); outcome != "passed" {
		t.Fatalf("verify %s: %s", outcome, out)
	}
	mustWrite(t, filepath.Join(repo, "a.go"), "package a\n")
	if _, err := runAutoformat(context.Background(), repo, []string{"a.go"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	sel, _ := st.recordedSelection()
	if sel.ResolvedImage == nil || sel.ResolvedImage.Digest != declDigestA || sel.DistinctImagesCount != 2 {
		t.Errorf("recorded = %+v count %d, want the verify gate's digest A and 2 distinct", sel.ResolvedImage, sel.DistinctImagesCount)
	}
}

// TestRun_FetchedGateContainerIsDeclared: run() threads the prompt's
// gate_container out of fetchPromptToFile and declares it before any gate.
// Deleting the declare call in run() turns this red.
func TestRun_FetchedGateContainerIsDeclared(t *testing.T) {
	for _, k := range []string{gateImageEnvVar, deploymentProfileEnvVar, gateServicesEnvVar, gatePostgresImageEnvVar, gateImageAllowlistEnvVar, gateBuildEnvVar} {
		t.Setenv(k, "")
	}
	t.Setenv(gateIsolationModeEnvVar, "clone")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID: "22222222-3333-4444-5555-666666666666", StageType: "implement",
		Prompt: "do the thing", PromptHash: "deadbeef",
		GateContainer: stageImage("ghcr.io/o/g@" + declDigestA),
	}
	withFakeUploader(t, fu)
	var stderr strings.Builder
	if got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
	}, &stderr); got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	want := `"event":"gate_container_declared","source":"stage","image":"ghcr.io/o/g@` + declDigestA + `"`
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("log lacks %s:\n%s", want, stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Persistent gate cache volume (E51.18 / #3967): under FISHHAWK_GATE_CACHE=
// process the container path mounts ONE per-runner-process named volume for
// GOCACHE and the lint cache, created + prepared + write-checked through
// execGateAuxArgvFn before EVERY exec, degraded to the per-exec binds on any
// failure, never reused across a (run, stage) pair, and removed at cleanup.
// ---------------------------------------------------------------------------

const (
	cacheRunA   = "aaaaaaaa-0000-4000-8000-000000000001"
	cacheStageA = "aaaaaaaa-0000-4000-8000-0000000000a1"
	cacheRunB   = "bbbbbbbb-0000-4000-8000-000000000002"
	cacheStageB = "bbbbbbbb-0000-4000-8000-0000000000b2"
	cacheSock   = "/nonexistent/cache-daemon.sock"
)

// cacheState is a mode=container state (img:1, safe docker at cacheSock)
// with FISHHAWK_GATE_CACHE=process plus extra, bound to (run, stage) when run
// is non-empty. It is NOT installed: a test swaps gateIsolation itself so it
// can drive two states in one process.
func cacheState(t *testing.T, logSink io.Writer, run, stage string, extra map[string]string) *gateIsolationState {
	t.Helper()
	env := map[string]string{gateIsolationModeEnvVar: "container", gateImageEnvVar: "img:1", gateCacheEnvVar: "process"}
	for k, v := range extra {
		env[k] = v
	}
	st, err := configureGateIsolation(fakeEnv(env), gateiso.Probes{}, logSink)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return safeDockerRuntime(cacheSock) }
	st.probeSandbox = func(context.Context) (bool, string) { return false, "no sandbox" }
	if run != "" {
		st.bindOwner(run, stage)
	}
	t.Cleanup(st.removeDockerConfig)
	return st
}

// execOn runs one container gate exec with st as the process-wide state.
func execOn(t *testing.T, st *gateIsolationState, extraEnv ...string) (string, int, gateDisposition) {
	t.Helper()
	prev := gateIsolation
	gateIsolation = st
	defer func() { gateIsolation = prev }()
	return runBoundedGateArgvDisposed(context.Background(), []string{"sh", "-c", "true"}, t.TempDir(),
		filepath.Join(t.TempDir(), "lc"), time.Minute, extraEnv...)
}

// cacheMountSource returns the source of the gate argv's -v token whose
// target is exactly target ("" when absent).
func cacheMountSource(argv []string, target string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-v" {
			if src, ok := strings.CutSuffix(argv[i+1], ":"+target); ok {
				return src
			}
		}
	}
	return ""
}

// envIndex returns the argv index of the LAST `-e key=…` token (-1 if none).
func envIndex(argv []string, key string) int {
	idx := -1
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-e" && strings.HasPrefix(argv[i+1], key+"=") {
			idx = i
		}
	}
	return idx
}

// endpointBound reports whether a recorded runtime argv opens with the
// docker endpoint binding to cacheSock.
func endpointBound(argv []string) bool {
	return len(argv) > 2 && argv[0] == "docker" && argv[1] == "--host" && argv[2] == "unix://"+cacheSock
}

// TestRunGateInContainer_CacheVolumeMountedAndEnvPinned is the done-means row
// for the shipped default: a process-mode state creates, prepares and
// write-checks the volume (each endpoint-bound, all BEFORE the gate run),
// mounts it read-write at /gatecache in place of the per-exec /gocache and
// /lintcache binds, points GOCACHE / GOLANGCI_LINT_CACHE into it, and — with
// the postgres service — still applies the service DSN LAST.
func TestRunGateInContainer_CacheVolumeMountedAndEnvPinned(t *testing.T) {
	rows := []struct {
		name      string
		extra     map[string]string
		wantOrder string
	}{
		{"no services", nil, "passwd,cache_create,cache_prepare,cache_check,gate_run"},
		{"postgres service", map[string]string{gateServicesEnvVar: "postgres"},
			"passwd,cache_create,cache_prepare,cache_check,volume_create,service_run,logs,ready,bootstrap,gate_run,service_rm,volume_rm"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			st := cacheState(t, &log, cacheRunA, cacheStageA, r.extra)
			t.Cleanup(st.cleanup)
			g := scriptGateExec(t, nil)
			if out, code, disp := execOn(t, st, "GOCACHE=/evil", "FISHHAWK_TEST_PG_URL=postgres://attacker@evil/x"); code != 0 || disp != gateExecuted {
				t.Fatalf("(out, code, disp) = (%q, %d, %s)", out, code, disp)
			}
			if got := strings.Join(g.order, ","); got != r.wantOrder {
				t.Fatalf("call order:\n got %s\nwant %s", got, r.wantOrder)
			}
			name := g.argv["cache_create"][len(g.argv["cache_create"])-1]
			if !(gateiso.CacheVolume{Name: name}).BelongsTo(cacheRunA, cacheStageA) {
				t.Fatalf("cache volume %q does not embed the bound run/stage", name)
			}
			for _, l := range []string{"cache_create", "cache_prepare", "cache_check"} {
				if !endpointBound(g.argv[l]) {
					t.Errorf("%s argv is not endpoint-bound: %q", l, g.argv[l])
				}
				if !strings.Contains(strings.Join(g.argv[l], " "), name) {
					t.Errorf("%s argv does not name %s: %q", l, name, g.argv[l])
				}
			}
			for _, l := range []string{"cache_prepare", "cache_check"} {
				if got := gateRunImage(g.argv[l]); got != "img:1" {
					t.Errorf("%s ran image %q, want the gate image img:1", l, got)
				}
			}
			prep := g.argv["cache_prepare"]
			if owner := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()); !argvHasPair(prep, "--user", "0:0") || !argvHasPair(prep, "sh", owner) {
				t.Errorf("prepare does not run as root and chown to the caller %s: %q", owner, prep)
			}
			if !argvHasPair(g.argv["cache_check"], "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())) {
				t.Errorf("write check does not run under the gate's user pin: %q", g.argv["cache_check"])
			}
			gate := g.argv["gate_run"]
			if src := cacheMountSource(gate, gateiso.MountGateCache); src != name {
				t.Errorf("gate /gatecache source = %q, want the volume %q (read-write): %q", src, name, gate)
			}
			for _, target := range []string{gateiso.MountGoCache, gateiso.MountLintCache} {
				if src := cacheMountSource(gate, target); src != "" {
					t.Errorf("volume mode still binds %s from %q", target, src)
				}
			}
			if v, _ := lastEnvValue(gate, "GOCACHE"); v != gateiso.GateGoCache {
				t.Errorf("gate GOCACHE = %q, want %q (an extra must not win)", v, gateiso.GateGoCache)
			}
			if v, _ := lastEnvValue(gate, "GOLANGCI_LINT_CACHE"); v != gateiso.GateLintCache {
				t.Errorf("gate GOLANGCI_LINT_CACHE = %q, want %q", v, gateiso.GateLintCache)
			}
			if r.extra != nil {
				if v, _ := lastEnvValue(gate, "FISHHAWK_TEST_PG_URL"); v != gateiso.PostgresGateURL {
					t.Errorf("gate DSN = %q, want %q", v, gateiso.PostgresGateURL)
				}
				if envIndex(gate, "FISHHAWK_TEST_PG_URL") < envIndex(gate, "GOCACHE") {
					t.Errorf("the service env must stay LAST, after the cache env: %q", gate)
				}
			}
			if !strings.Contains(log.String(), `"event":"gate_cache_volume_ready","volume":"`+name+`","reused":false`) {
				t.Errorf("missing gate_cache_volume_ready:\n%s", log.String())
			}
		})
	}
}

// TestRunGateInContainer_CacheVolumeReusedWithinProcess: two execs on one
// state mount the SAME volume; create + prepare + check run once per exec.
func TestRunGateInContainer_CacheVolumeReusedWithinProcess(t *testing.T) {
	var log strings.Builder
	st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
	t.Cleanup(st.cleanup)
	g := scriptGateExec(t, nil)
	var mounted []string
	for i := 0; i < 2; i++ {
		if _, code, disp := execOn(t, st); code != 0 || disp != gateExecuted {
			t.Fatalf("exec %d = %d / %s", i+1, code, disp)
		}
		mounted = append(mounted, cacheMountSource(g.argv["gate_run"], gateiso.MountGateCache))
	}
	if mounted[0] == "" || mounted[0] != mounted[1] {
		t.Fatalf("execs mounted %q, want the same volume twice", mounted)
	}
	for _, l := range []string{"cache_create", "cache_prepare", "cache_check", "gate_run"} {
		if g.counts[l] != 2 {
			t.Errorf("%s ran %d times, want once per exec (2)", l, g.counts[l])
		}
	}
	if !strings.Contains(log.String(), `"volume":"`+mounted[0]+`","reused":true`) {
		t.Errorf("the second exec must log reused:true:\n%s", log.String())
	}
}

// TestGateCacheVolume_DistinctPerProcessNeverShared is the poisoning
// boundary: two states standing for two runner processes — bound to the SAME
// (run, stage), the retried-stage shape — mint DIFFERENT volumes; B's gate
// and aux argvs never name A's; A.cleanup() removes exactly A's; B's next
// exec still mounts B's.
func TestGateCacheVolume_DistinctPerProcessNeverShared(t *testing.T) {
	a := cacheState(t, io.Discard, cacheRunA, cacheStageA, nil)
	b := cacheState(t, io.Discard, cacheRunA, cacheStageA, nil)
	t.Cleanup(a.cleanup)
	t.Cleanup(b.cleanup)
	ga := scriptGateExec(t, nil)
	if _, code, _ := execOn(t, a); code != 0 {
		t.Fatalf("A exec = %d", code)
	}
	nameA := cacheMountSource(ga.argv["gate_run"], gateiso.MountGateCache)
	gb := scriptGateExec(t, nil)
	if _, code, _ := execOn(t, b); code != 0 {
		t.Fatalf("B exec = %d", code)
	}
	nameB := cacheMountSource(gb.argv["gate_run"], gateiso.MountGateCache)
	if nameA == "" || nameB == "" || nameA == nameB {
		t.Fatalf("A mounted %q, B mounted %q: two processes must never share a volume", nameA, nameB)
	}
	for l, argv := range gb.argv {
		if strings.Contains(strings.Join(argv, " "), nameA) {
			t.Errorf("B's %s argv names A's volume %s: %q", l, nameA, argv)
		}
	}
	gc := scriptAuxExec(t, nil)
	a.cleanup()
	if gc.counts["cache_rm"] != 1 || !argvHasPair(gc.argv["cache_rm"], "-f", nameA) || !endpointBound(gc.argv["cache_rm"]) {
		t.Fatalf("A.cleanup() = %d rm calls, last %q; want exactly one endpoint-bound `volume rm -f %s`", gc.counts["cache_rm"], gc.argv["cache_rm"], nameA)
	}
	g2 := scriptGateExec(t, nil)
	if _, code, _ := execOn(t, b); code != 0 {
		t.Fatalf("B exec 2 = %d", code)
	}
	if got := cacheMountSource(g2.argv["gate_run"], gateiso.MountGateCache); got != nameB {
		t.Errorf("B's next exec mounted %q, want its own %q", got, nameB)
	}
}

// TestGateCacheVolume_DifferentOwnerNeverReuses is #3967 approval condition
// 2's runtime guard: an exec for a different (run, stage) pair — the run
// differing, or only the stage — never mounts the volume minted for the first
// pair; it mints its own, embedding its own ids, and cleanup removes both.
func TestGateCacheVolume_DifferentOwnerNeverReuses(t *testing.T) {
	rows := []struct{ name, run, stage string }{
		{"different run and stage", cacheRunB, cacheStageB},
		{"same run, different stage", cacheRunA, cacheStageB},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
			g1 := scriptGateExec(t, nil)
			if _, code, _ := execOn(t, st); code != 0 {
				t.Fatalf("exec 1 = %d", code)
			}
			first := cacheMountSource(g1.argv["gate_run"], gateiso.MountGateCache)
			st.bindOwner(r.run, r.stage)
			g2 := scriptGateExec(t, nil)
			if _, code, _ := execOn(t, st); code != 0 {
				t.Fatalf("exec 2 = %d", code)
			}
			second := cacheMountSource(g2.argv["gate_run"], gateiso.MountGateCache)
			if first == "" || second == "" || first == second {
				t.Fatalf("pair 1 mounted %q, pair 2 mounted %q: a second (run, stage) must never reuse the first's volume", first, second)
			}
			if !(gateiso.CacheVolume{Name: second}).BelongsTo(r.run, r.stage) {
				t.Errorf("the second volume %q does not embed its own run/stage", second)
			}
			for l, argv := range g2.argv {
				if strings.Contains(strings.Join(argv, " "), first) {
					t.Errorf("pair 2's %s argv names pair 1's volume: %q", l, argv)
				}
			}
			if !strings.Contains(log.String(), `"event":"gate_cache_volume_not_reused","volume":"`+first+`","replacement":"`+second+`"`) {
				t.Errorf("missing gate_cache_volume_not_reused:\n%s", log.String())
			}
			gc := scriptAuxExec(t, nil)
			st.cleanup()
			if gc.counts["cache_rm"] != 2 {
				t.Errorf("cleanup removed %d volumes, want both minted (2)", gc.counts["cache_rm"])
			}
		})
	}
}

// TestGateCacheVolume_UnboundStateNeverMountsAnotherOwnersVolume: a state
// that never had bindOwner called — even one already HOLDING a volume minted
// for another (run, stage), seeded by construction — never mounts it: the
// mint refuses the empty owner and the exec degrades to the per-exec binds
// with no cache runtime call at all. bindOwner and gateCacheVolume are
// nil-safe.
func TestGateCacheVolume_UnboundStateNeverMountsAnotherOwnersVolume(t *testing.T) {
	other, err := gateiso.NewCacheVolume(cacheRunB, cacheStageB)
	if err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	st := cacheState(t, &log, "", "", nil)
	st.cacheVol = &other
	g := scriptGateExec(t, nil)
	if _, code, disp := execOn(t, st); code != 0 || disp != gateExecuted {
		t.Fatalf("exec = %d / %s", code, disp)
	}
	gate := g.argv["gate_run"]
	if strings.Contains(strings.Join(gate, " "), other.Name) || cacheMountSource(gate, gateiso.MountGateCache) != "" {
		t.Fatalf("an unbound state mounted a cache volume: %q", gate)
	}
	if cacheMountSource(gate, gateiso.MountGoCache) == "" {
		t.Errorf("the degrade must keep the per-exec /gocache bind: %q", gate)
	}
	if got := strings.Join(g.order, ","); got != "passwd,gate_run" {
		t.Errorf("calls = %s, want no cache runtime call", got)
	}
	if !strings.Contains(log.String(), `"event":"gate_cache_volume_unavailable","volume":"","step":"mint"`) {
		t.Errorf("missing the mint degrade:\n%s", log.String())
	}
	var nilState *gateIsolationState
	nilState.bindOwner(cacheRunA, cacheStageA)
	if got := nilState.gateCacheVolume(context.Background(), safeDockerRuntime(cacheSock), "img:1", t.TempDir(), nil, 1, 1); got != "" {
		t.Errorf("nil state gateCacheVolume = %q, want \"\"", got)
	}
	nilState.cleanup()
}

// TestRunGateInContainer_CacheVolumeDegrades: one row per cache-step failure
// mode. Each still EXECUTES the gate (gateExecuted, its own exit code) on the
// per-exec /gocache and /lintcache binds with GOCACHE=/gocache, never mounts
// /gatecache, logs gate_cache_volume_unavailable naming the step, and runs no
// later cache step; the minted name stays recorded so cleanup removes it.
func TestRunGateInContainer_CacheVolumeDegrades(t *testing.T) {
	failOn := func(label string, code int, timedOut bool) func(string, int) (string, int, bool) {
		return func(l string, n int) (string, int, bool) {
			if l == label {
				return "Error: boom", code, timedOut
			}
			return serviceHappyPath(l, n)
		}
	}
	rows := []struct {
		name    string
		respond func(string, int) (string, int, bool)
		uid     int
		step    string
		reason  string
		ran     string
	}{
		{"create exits 1", failOn("cache_create", 1, false), 0, "create", "exit 1: Error: boom", "cache_create"},
		{"create timed out", failOn("cache_create", -1, true), 0, "create", "timed out after " + gateCacheVolumeTimeout.String(), "cache_create"},
		{"prepare exits 1", failOn("cache_prepare", 1, false), 0, "prepare", "exit 1: Error: boom", "cache_create,cache_prepare"},
		{"prepare timed out", failOn("cache_prepare", -1, true), 0, "prepare", "timed out after " + gateCachePrepareTimeout.String(), "cache_create,cache_prepare"},
		{"check exits 1 (ownership)", failOn("cache_check", 1, false), 0, "check", "ownership: the gate uid", "cache_create,cache_prepare,cache_check"},
		{"prepare argv refused", nil, -1, "prepare", "argv: ", "cache_create"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
			if r.uid != 0 {
				st.uid, st.gid = r.uid, r.uid
			}
			g := scriptGateExec(t, r.respond)
			out, code, disp := execOn(t, st)
			if code != 0 || disp != gateExecuted || out != "gate ok" {
				t.Fatalf("(out, code, disp) = (%q, %d, %s), want the gate's own verdict", out, code, disp)
			}
			var cacheCalls []string
			for _, l := range g.order {
				if strings.HasPrefix(l, "cache_") {
					cacheCalls = append(cacheCalls, l)
				}
			}
			if got := strings.Join(cacheCalls, ","); got != r.ran {
				t.Errorf("cache calls = %s, want %s (no step after the failing one)", got, r.ran)
			}
			gate := g.argv["gate_run"]
			if src := cacheMountSource(gate, gateiso.MountGateCache); src != "" {
				t.Errorf("a degraded exec mounted the cache volume %q: %q", src, gate)
			}
			if cacheMountSource(gate, gateiso.MountGoCache) == "" || cacheMountSource(gate, gateiso.MountLintCache) == "" {
				t.Errorf("the degrade must keep the per-exec /gocache and /lintcache binds: %q", gate)
			}
			if v, _ := lastEnvValue(gate, "GOCACHE"); v != gateiso.MountGoCache {
				t.Errorf("gate GOCACHE = %q, want the per-exec %q", v, gateiso.MountGoCache)
			}
			if !strings.Contains(log.String(), `"event":"gate_cache_volume_unavailable"`) ||
				!strings.Contains(log.String(), `"step":"`+r.step+`","reason":"`+r.reason) {
				t.Errorf("log lacks the %s degrade with reason %q:\n%s", r.step, r.reason, log.String())
			}
			if !strings.Contains(log.String(), `"event":"gate_container_timing","cache":"degraded","cache_volume":""`) {
				t.Errorf("timing line must report the degraded posture:\n%s", log.String())
			}
			gc := scriptAuxExec(t, nil)
			st.cleanup()
			if gc.counts["cache_rm"] != 1 {
				t.Errorf("cleanup ran %d volume rm, want 1 (a partial create is still removed)", gc.counts["cache_rm"])
			}
		})
	}
}

// TestRunGateInContainer_CacheOffKeepsPerExecCaches: FISHHAWK_GATE_CACHE=off
// and the ZERO-VALUE cache mode of a hand-built state both run no cache
// runtime call and keep the pre-#3967 per-exec binds.
func TestRunGateInContainer_CacheOffKeepsPerExecCaches(t *testing.T) {
	for _, name := range []string{"off", "zero value"} {
		t.Run(name, func(t *testing.T) {
			var log strings.Builder
			st := cacheState(t, &log, cacheRunA, cacheStageA, map[string]string{gateCacheEnvVar: "off"})
			if name == "zero value" {
				st.cacheMode = ""
			}
			g := scriptGateExec(t, nil)
			if _, code, disp := execOn(t, st); code != 0 || disp != gateExecuted {
				t.Fatalf("exec = %d / %s", code, disp)
			}
			if got := strings.Join(g.order, ","); got != "passwd,gate_run" {
				t.Errorf("calls = %s, want passwd,gate_run", got)
			}
			gate := g.argv["gate_run"]
			if cacheMountSource(gate, gateiso.MountGateCache) != "" || cacheMountSource(gate, gateiso.MountGoCache) == "" || cacheMountSource(gate, gateiso.MountLintCache) == "" {
				t.Errorf("off must keep the per-exec binds only: %q", gate)
			}
			if !strings.Contains(log.String(), `"event":"gate_container_timing","cache":"off"`) {
				t.Errorf("timing line must report cache off:\n%s", log.String())
			}
			gc := scriptAuxExec(t, nil)
			st.cleanup()
			if len(gc.order) != 0 {
				t.Errorf("cleanup with no minted volume ran %q", gc.order)
			}
		})
	}
}

// TestGateIsolationState_CleanupRemovesCacheVolume: cleanup removes the
// minted volume with exactly one endpoint-bound `volume rm -f <name>`; with
// no minted volume it runs nothing; a failing removal is logged as
// gate_cache_volume_cleanup_failed (naming the label) and the global is
// still cleared.
func TestGateIsolationState_CleanupRemovesCacheVolume(t *testing.T) {
	t.Run("removes after an exec", func(t *testing.T) {
		var log strings.Builder
		st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
		g := scriptGateExec(t, nil)
		if _, code, _ := execOn(t, st); code != 0 {
			t.Fatalf("exec = %d", code)
		}
		name := cacheMountSource(g.argv["gate_run"], gateiso.MountGateCache)
		gc := scriptAuxExec(t, nil)
		installGateState(t, st)
		st.cleanup()
		if got := strings.Join(gc.order, ","); got != "cache_rm" {
			t.Fatalf("cleanup calls = %s, want exactly one cache_rm", got)
		}
		want := []string{"docker", "--host", "unix://" + cacheSock, "volume", "rm", "-f", name}
		if strings.Join(gc.argv["cache_rm"], "\x00") != strings.Join(want, "\x00") {
			t.Errorf("rm argv = %q, want %q", gc.argv["cache_rm"], want)
		}
		if gateIsolation != nil || !strings.Contains(log.String(), `"event":"gate_cache_volume_removed","volume":"`+name+`"`) {
			t.Errorf("global = %v; log:\n%s", gateIsolation, log.String())
		}
		st.cleanup()
		if gc.counts["cache_rm"] != 1 {
			t.Errorf("a second cleanup re-removed (%d rm calls)", gc.counts["cache_rm"])
		}
	})
	t.Run("no exec, no removal", func(t *testing.T) {
		st := cacheState(t, io.Discard, cacheRunA, cacheStageA, nil)
		gc := scriptAuxExec(t, nil)
		st.cleanup()
		if len(gc.order) != 0 {
			t.Errorf("cleanup with nothing minted ran %q", gc.order)
		}
	})
	t.Run("unbound runtime: create and removal argv refused, nothing runs", func(t *testing.T) {
		var log strings.Builder
		st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
		g := scriptAuxExec(t, nil)
		unbound := gateiso.Runtime{Kind: gateiso.KindDocker}
		if got := st.gateCacheVolume(context.Background(), unbound, "img:1", t.TempDir(), nil, 1, 1); got != "" {
			t.Fatalf("an unbound runtime mounted %q", got)
		}
		st.cleanup()
		if len(g.order) != 0 {
			t.Errorf("a refused argv still ran %q", g.order)
		}
		for _, w := range []string{`"step":"create","reason":"argv: `, `"event":"gate_cache_volume_cleanup_failed","volume":"fishhawk-gate-cache-`, `"detail":"argv: `} {
			if !strings.Contains(log.String(), w) {
				t.Errorf("log lacks %s:\n%s", w, log.String())
			}
		}
	})
	t.Run("removal failure logged, global cleared", func(t *testing.T) {
		var log strings.Builder
		st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
		g := scriptGateExec(t, nil)
		if _, code, _ := execOn(t, st); code != 0 {
			t.Fatalf("exec = %d", code)
		}
		name := cacheMountSource(g.argv["gate_run"], gateiso.MountGateCache)
		scriptAuxExec(t, func(string, int) (string, int, bool) { return "Error: volume is in use", 1, false })
		installGateState(t, st)
		st.cleanup()
		if gateIsolation != nil {
			t.Errorf("a failed removal must still clear the global")
		}
		want := `"event":"gate_cache_volume_cleanup_failed","volume":"` + name + `","detail":"exit 1: Error: volume is in use","label":"` + gateiso.CacheVolumeLabel + `"`
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, log.String())
		}
	})
}

// TestRunGateInContainer_TimingLogLine: every container exec logs exactly one
// gate_container_timing line carrying the cache posture, the volume, the
// phase split and the gate's exit code.
func TestRunGateInContainer_TimingLogLine(t *testing.T) {
	var log strings.Builder
	st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
	t.Cleanup(st.cleanup)
	scriptGateExec(t, func(l string, n int) (string, int, bool) {
		if l == "gate_run" && n == 2 {
			return "red", 3, false
		}
		return serviceHappyPath(l, n)
	})
	for i := 0; i < 2; i++ {
		execOn(t, st)
	}
	type timing struct {
		Event       string `json:"event"`
		Cache       string `json:"cache"`
		CacheVolume string `json:"cache_volume"`
		CacheMS     *int64 `json:"cache_ms"`
		SeedMS      *int64 `json:"seed_ms"`
		ServiceMS   *int64 `json:"service_ms"`
		ExecMS      *int64 `json:"exec_ms"`
		ExitCode    int    `json:"exit_code"`
	}
	var lines []timing
	for _, l := range strings.Split(log.String(), "\n") {
		if strings.Contains(l, `"event":"gate_container_timing"`) {
			var tl timing
			if err := json.Unmarshal([]byte(l), &tl); err != nil {
				t.Fatalf("timing line is not JSON: %v: %s", err, l)
			}
			lines = append(lines, tl)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("got %d timing lines, want one per exec (2):\n%s", len(lines), log.String())
	}
	for i, tl := range lines {
		if tl.Cache != "process" || !strings.HasPrefix(tl.CacheVolume, "fishhawk-gate-cache-") ||
			tl.CacheMS == nil || tl.SeedMS == nil || tl.ServiceMS == nil || tl.ExecMS == nil {
			t.Errorf("timing line %d = %+v", i+1, tl)
		}
	}
	if lines[0].ExitCode != 0 || lines[1].ExitCode != 3 {
		t.Errorf("exit codes = %d, %d; want 0, 3", lines[0].ExitCode, lines[1].ExitCode)
	}
}

// TestDeclaredImage_CacheVolumePreparedForResolvedImage (#3967 approval
// condition 5): for a spec-declared gate_container the cache volume is
// prepared and checked with the RESOLVED name@digest — the image the gate
// itself runs — and mounted beside it, exactly as for FISHHAWK_GATE_IMAGE.
func TestDeclaredImage_CacheVolumePreparedForResolvedImage(t *testing.T) {
	ref := "ghcr.io/o/g@" + declDigestA
	st := declaredState(t, containerEnv(map[string]string{gateImageEnvVar: "img:1", gateCacheEnvVar: "process"}),
		safeDockerRuntime(declSock), stageImage(ref), io.Discard)
	st.bindOwner(cacheRunA, cacheStageA)
	t.Cleanup(st.cleanup)
	g := scriptGateExec(t, pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA))
	if _, code, disp := runDeclaredGate(t, context.Background(), t.TempDir()); code != 0 || disp != gateExecuted {
		t.Fatalf("exec = %d / %s", code, disp)
	}
	if got, want := strings.Join(g.order, ","), "inspect,passwd,cache_create,cache_prepare,cache_check,gate_run"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	for _, l := range []string{"cache_prepare", "cache_check", "gate_run"} {
		if got := gateRunImage(g.argv[l]); got != ref {
			t.Errorf("%s image = %q, want the resolved %q (never img:1)", l, got, ref)
		}
	}
	if src := cacheMountSource(g.argv["gate_run"], gateiso.MountGateCache); !strings.HasPrefix(src, "fishhawk-gate-cache-") {
		t.Errorf("declared-image gate does not mount the cache volume: %q", g.argv["gate_run"])
	}
}

// ---------------------------------------------------------------------------
// Runner docker config (E51.26 / #4046): every post-selection runtime call is
// pinned to a docker config the runner chose — the runner-owned anonymous
// config by default, FISHHAWK_GATE_DOCKER_CONFIG by opt-in — and an opt-in
// config's matching credential helpers are probed under a bound before any
// pull or build.
// ---------------------------------------------------------------------------

// credentialHelpersOnPath writes docker-credential-<name> scripts into a
// fresh dir and prepends it to PATH (the runtime CLI env inherits PATH).
func credentialHelpersOnPath(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		p := filepath.Join(dir, "docker-credential-"+name)
		mustWrite(t, p, "#!/bin/sh\n"+body+"\n")
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// operatorDockerConfig is an operator-prepared docker config dir holding
// content as its config.json.
func operatorDockerConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "config.json"), content)
	return dir
}

// shortCredentialProbe shrinks the probe bound for the test.
func shortCredentialProbe(t *testing.T, d time.Duration) {
	t.Helper()
	prev := gateCredentialProbeTimeout
	gateCredentialProbeTimeout = d
	t.Cleanup(func() { gateCredentialProbeTimeout = prev })
}

// orderProbes wraps the REAL credential probe so each probe lands in g.order
// as "probe:<helper>" — the ordering against the runtime calls it guards.
func orderProbes(t *testing.T, g *gateExecScript) {
	t.Helper()
	prev := probeCredentialHelperFn
	probeCredentialHelperFn = func(ctx context.Context, p gateiso.CredentialProbe, env []string, bound time.Duration) gateiso.ProbeResult {
		g.order = append(g.order, "probe:"+p.Executable())
		return prev(ctx, p, env, bound)
	}
	t.Cleanup(func() { probeCredentialHelperFn = prev })
}

// envOf returns the value of key in env and whether it is present.
func envOf(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

// TestRunGateInContainer_EveryRuntimeCallPinsRunnerDockerConfig: with an
// inherited DOCKER_CONFIG / REGISTRY_AUTH_FILE pointing at the operator's
// interactive store, EVERY runtime call of a container exec — the declared
// pull and inspect, the passwd read, the cache volume steps, the postgres
// service lifecycle, the gate run, its `rm -f` kill, and the cache volume
// removal at cleanup — carries DOCKER_CONFIG=<the runner-owned dir> and never
// the inherited value. A second exec reuses the same minted dir. Passing
// os.Environ() instead of the bound env in runGateInContainer turns this red.
func TestRunGateInContainer_EveryRuntimeCallPinsRunnerDockerConfig(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "/operator/interactive/.docker")
	t.Setenv("REGISTRY_AUTH_FILE", "/operator/auth.json")
	var log strings.Builder
	st := cacheState(t, &log, cacheRunA, cacheStageA, map[string]string{gateServicesEnvVar: "postgres"})
	st.declare(stageImage("ghcr.io/o/g:v1"))
	shortServiceReady(t, time.Second)
	pulled := pulledImageResponder(0, declImageID+" ghcr.io/o/g@"+declDigestA, declImageID+" ghcr.io/o/g@"+declDigestA)
	g := scriptGateExec(t, func(label string, n int) (string, int, bool) {
		if label == "gate_run" {
			return "", -1, false
		}
		return pulled(label, n)
	})
	type call struct{ label, cfg string }
	var calls []call
	record := func(prev func(context.Context, []string, string, []string, time.Duration) (string, int, bool)) func(context.Context, []string, string, []string, time.Duration) (string, int, bool) {
		return func(ctx context.Context, argv []string, dir string, env []string, d time.Duration) (string, int, bool) {
			cfg, _ := envOf(env, "DOCKER_CONFIG")
			if v, ok := envOf(env, "REGISTRY_AUTH_FILE"); ok {
				t.Errorf("%s carries REGISTRY_AUTH_FILE=%q on docker", classifyGateArgv(argv), v)
			}
			calls = append(calls, call{classifyGateArgv(argv), cfg})
			return prev(ctx, argv, dir, env, d)
		}
	}
	prevAux, prevHost := execGateAuxArgvFn, execBoundedHostArgvFn
	execGateAuxArgvFn, execBoundedHostArgvFn = record(prevAux), record(prevHost)
	t.Cleanup(func() { execGateAuxArgvFn, execBoundedHostArgvFn = prevAux, prevHost })
	for i := 0; i < 2; i++ {
		if _, code, _ := execOn(t, st); code != -1 {
			t.Fatalf("exec %d exit %d, want the scripted -1", i+1, code)
		}
	}
	minted := st.dockerConfig.Dir
	st.cleanup()
	want := map[string]bool{"pull": true, "inspect": true, "passwd": true, "cache_create": true, "cache_prepare": true, "cache_check": true,
		"volume_create": true, "service_run": true, "logs": true, "ready": true, "bootstrap": true, "gate_run": true, "gate_kill": true,
		"service_rm": true, "volume_rm": true, "cache_rm": true}
	for _, c := range calls {
		delete(want, c.label)
		if c.cfg != minted {
			t.Errorf("%s ran with DOCKER_CONFIG=%q, want the runner-owned %q", c.label, c.cfg, minted)
		}
	}
	if len(want) != 0 {
		t.Errorf("fixture covers no %v call (order %q)", want, g.order)
	}
	if n := strings.Count(log.String(), `"event":"gate_docker_config_minted"`); n != 1 {
		t.Errorf("minted %d docker configs for two execs on one state, want 1:\n%s", n, log.String())
	}
}

// TestRunGateInContainer_BlockedCredentialHelperFailsFast is probe site (a):
// an opt-in config whose credsStore matches the gate image's registry and
// never answers fails the exec within the bound — gateUnavailable naming
// container_credentials_blocked, the helper and the server — with NO runtime
// call before or after the probe. The helper's whole process group is gone.
func TestRunGateInContainer_BlockedCredentialHelperFailsFast(t *testing.T) {
	helpers := credentialHelpersOnPath(t, map[string]string{"fhblock": `echo $$ > "` + "$FH_HELPER_DIR" + `/helper.pid"; sleep 600`})
	t.Setenv("FH_HELPER_DIR", helpers)
	// 2s, not the 300ms other rows use: this row reads the helper's pid, which
	// a loaded -race package run can delay past a sub-second bound.
	shortCredentialProbe(t, 2*time.Second)
	var log strings.Builder
	st := containerStateEnv(t, "img:1", declSock, &log, map[string]string{gateDockerConfigEnvVar: operatorDockerConfig(t, `{"credsStore":"fhblock"}`)})
	installGateState(t, st)
	g := scriptGateExec(t, nil)
	orderProbes(t, g)
	start := time.Now()
	out, code, disp := runDeclaredGate(t, context.Background(), t.TempDir())
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("blocked helper held the exec %s", elapsed)
	}
	if code != -1 || disp != gateUnavailable {
		t.Fatalf("exec = %d / %s, want -1 / unavailable: %s", code, disp, out)
	}
	if want := "gate container: container_credentials_blocked: credential helper docker-credential-fhblock did not answer within 2s for https://index.docker.io/v1/"; !strings.HasPrefix(out, want) {
		t.Errorf("output = %q, want lead %q", out, want)
	}
	if got := strings.Join(g.order, ","); got != "probe:docker-credential-fhblock" {
		t.Errorf("calls = %s, want only the probe (no runtime call may run behind a blocked helper)", got)
	}
	if !strings.Contains(log.String(), `"event":"gate_credentials_probe","site":"gate","helper":"docker-credential-fhblock","server":"https://index.docker.io/v1/","outcome":"blocked"`) {
		t.Errorf("missing blocked gate_credentials_probe line:\n%s", log.String())
	}
	b, err := os.ReadFile(filepath.Join(helpers, "helper.pid"))
	if err != nil {
		t.Fatalf("fixture: the helper never ran: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("helper pid %d survived the probe (kill(0) = %v)", pid, err)
	}
}

// TestRunVerifyFixLoop_BlockedCredentialHelperIsCategoryC: through the fix
// loop a blocked helper is category C — verify_gate_unavailable, the verify
// command never executed, the fix agent never invoked.
func TestRunVerifyFixLoop_BlockedCredentialHelperIsCategoryC(t *testing.T) {
	credentialHelpersOnPath(t, map[string]string{"fhblock": "sleep 600"})
	shortCredentialProbe(t, 300*time.Millisecond)
	installGateState(t, containerStateEnv(t, "img:1", declSock, io.Discard,
		map[string]string{gateDockerConfigEnvVar: operatorDockerConfig(t, `{"credsStore":"fhblock"}`)}))
	g := scriptGateExec(t, nil)
	cfg, logPath := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	cfg.verifyMaxIterations = 2
	res := agent.Result{OK: true}
	var log strings.Builder
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	if _, _, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &log); err != nil {
		t.Fatal(err)
	}
	if res.OK || res.FailureCategory != "C" || !strings.Contains(res.FailureReason, "container_credentials_blocked") {
		t.Errorf("res = OK:%t cat:%q reason:%q, want category C naming container_credentials_blocked", res.OK, res.FailureCategory, res.FailureReason)
	}
	if invoker.callIdx != 0 || len(g.order) != 0 || len(readVerifyFormLog(t, logPath)) != 0 {
		t.Errorf("fix agent %d / runtime calls %q / verify runs after a blocked helper", invoker.callIdx, g.order)
	}
	if !strings.Contains(log.String(), `"event":"verify_gate_unavailable"`) {
		t.Errorf("log lacks verify_gate_unavailable:\n%s", log.String())
	}
}

// TestResolveBuiltImage_BlockedBaseHelperFailsBeforeBuild is probe site (b)
// (#4046 approval condition 2): a declared build whose Dockerfile base
// matches a blocked helper fails gateUnavailable after the content-addressed
// inspect MISS (allowed — it is what makes a build necessary) and BEFORE the
// build: no runtime call runs after the probe, and above all no build. The
// gate image probe site (a) matches nothing here (a build has no gate image).
func TestResolveBuiltImage_BlockedBaseHelperFailsBeforeBuild(t *testing.T) {
	repo := buildRepo(t, map[string]string{"gate/Dockerfile": "FROM ghcr.io/o/base:1\nCOPY . /src\n", "gate/a.txt": "x\n"})
	credentialHelpersOnPath(t, map[string]string{"fhblock": "sleep 600"})
	shortCredentialProbe(t, 300*time.Millisecond)
	var log strings.Builder
	declaredState(t, containerEnv(map[string]string{gateDockerConfigEnvVar: operatorDockerConfig(t, `{"credHelpers":{"ghcr.io":"fhblock"}}`)}),
		safeDockerRuntime(declSock), declaredBuild, &log)
	var g *gateExecScript
	g = scriptGateExec(t, buildResponder(&g, 0, nil))
	orderProbes(t, g)
	out, code, disp := runDeclaredGate(t, context.Background(), repo)
	if code != -1 || disp != gateUnavailable {
		t.Fatalf("exec = %d / %s, want -1 / unavailable: %s", code, disp, out)
	}
	if !strings.Contains(out, "gate_container unavailable: container_credentials_blocked: credential helper docker-credential-fhblock did not answer within 300ms for ghcr.io") {
		t.Errorf("output = %q", out)
	}
	probeAt := slices.Index(g.order, "probe:docker-credential-fhblock")
	if probeAt < 0 || probeAt != len(g.order)-1 {
		t.Errorf("calls = %q: the probe must run and be the LAST call (nothing after a blocked probe)", g.order)
	}
	if g.counts["build"] != 0 {
		t.Errorf("the build ran behind a blocked base helper: %q", g.order)
	}
	if !strings.Contains(log.String(), `"site":"build","helper":"docker-credential-fhblock","server":"ghcr.io","outcome":"blocked"`) {
		t.Errorf("missing build-site probe line:\n%s", log.String())
	}
}

// TestResolveBuiltImage_ProbeSiteParseErrorFailsClosed (#4046 review): a
// Dockerfile the build-base probe site cannot parse — unreachable today, since
// ScreenDockerfile parsed the same bytes, so the divergence is injected — is
// gateUnavailable naming the parse, never a silent skip of the base-helper
// probe straight into the build.
func TestResolveBuiltImage_ProbeSiteParseErrorFailsClosed(t *testing.T) {
	repo := buildRepo(t, map[string]string{"gate/Dockerfile": "FROM ghcr.io/o/base:1\nCOPY . /src\n", "gate/a.txt": "x\n"})
	prev := parseGateDockerfileFn
	parseGateDockerfileFn = func([]byte) (*gateiso.Dockerfile, error) { return nil, errors.New("injected parser divergence") }
	t.Cleanup(func() { parseGateDockerfileFn = prev })
	declaredState(t, containerEnv(nil), safeDockerRuntime(declSock), declaredBuild, nil)
	var g *gateExecScript
	g = scriptGateExec(t, buildResponder(&g, 0, nil))
	out, code, disp := runDeclaredGate(t, context.Background(), repo)
	if code != -1 || disp != gateUnavailable {
		t.Fatalf("exec = %d / %s, want -1 / unavailable: %s", code, disp, out)
	}
	if !strings.Contains(out, "parse gate/Dockerfile for the credential probe: injected parser divergence") {
		t.Errorf("output = %q, want the probe-site parse failure named", out)
	}
	if g.counts["build"] != 0 {
		t.Errorf("the build ran past an unparsable probe site: %q", g.order)
	}
}

// TestRunGateInContainer_CancelledProbeIsNotBlocked (#4046 review): a probe
// cancelled by the stage's own context fails the exec naming the
// cancellation, never container_credentials_blocked (which names a locked
// screen that did not occur), and no runtime call runs after it.
func TestRunGateInContainer_CancelledProbeIsNotBlocked(t *testing.T) {
	var log strings.Builder
	st := containerStateEnv(t, "img:1", declSock, &log, map[string]string{gateDockerConfigEnvVar: operatorDockerConfig(t, `{"credsStore":"fhblock"}`)})
	installGateState(t, st)
	g := scriptGateExec(t, nil)
	prev := probeCredentialHelperFn
	probeCredentialHelperFn = func(_ context.Context, p gateiso.CredentialProbe, _ []string, bound time.Duration) gateiso.ProbeResult {
		g.order = append(g.order, "probe:"+p.Executable())
		return gateiso.ProbeResult{Probe: p, Outcome: gateiso.ProbeCancelled, Detail: context.Canceled.Error(), Bound: bound}
	}
	t.Cleanup(func() { probeCredentialHelperFn = prev })
	out, code, disp := runDeclaredGate(t, context.Background(), t.TempDir())
	if code != -1 || disp != gateUnavailable {
		t.Fatalf("exec = %d / %s, want -1 / unavailable: %s", code, disp, out)
	}
	if want := "gate container: credential helper docker-credential-fhblock probe for https://index.docker.io/v1/ cancelled: context canceled"; !strings.HasPrefix(out, want) {
		t.Errorf("output = %q, want lead %q", out, want)
	}
	if strings.Contains(out, "container_credentials_blocked") {
		t.Errorf("a cancelled probe was reported as a blocked helper: %q", out)
	}
	if got := strings.Join(g.order, ","); got != "probe:docker-credential-fhblock" {
		t.Errorf("calls = %s, want only the probe", got)
	}
	if !strings.Contains(log.String(), `"helper":"docker-credential-fhblock","server":"https://index.docker.io/v1/","outcome":"cancelled","detail":"context canceled"`) {
		t.Errorf("missing cancelled gate_credentials_probe line:\n%s", log.String())
	}
}

// TestResolveBuiltImage_CacheHitProbesNothing: a content-addressed cache hit
// builds nothing, so no base helper is probed (site (b) sits after the miss).
func TestResolveBuiltImage_CacheHitProbesNothing(t *testing.T) {
	repo := buildRepo(t, map[string]string{"gate/Dockerfile": "FROM ghcr.io/o/base:1\n"})
	credentialHelpersOnPath(t, map[string]string{"fhblock": "sleep 600"})
	shortCredentialProbe(t, 300*time.Millisecond)
	declaredState(t, containerEnv(map[string]string{gateDockerConfigEnvVar: operatorDockerConfig(t, `{"credHelpers":{"ghcr.io":"fhblock"}}`)}),
		safeDockerRuntime(declSock), declaredBuild, nil)
	g := scriptGateExec(t, func(label string, n int) (string, int, bool) {
		if label == "inspect" {
			return declImageID, 0, false
		}
		return serviceHappyPath(label, n)
	})
	orderProbes(t, g)
	if out, code, _ := runDeclaredGate(t, context.Background(), repo); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := strings.Join(g.order, ","); got != "inspect,passwd,gate_run" {
		t.Errorf("calls = %s, want a cache hit with no probe", got)
	}
}

// TestRunGateInContainer_UnmatchedHelperNotProbed: an opt-in config whose only
// helper serves a registry no image of this exec names (an ECR credHelpers
// entry, the gate image on Docker Hub) never runs that helper — no probe, no
// probe line — and the exec proceeds under the operator's config, which
// cleanup never removes.
func TestRunGateInContainer_UnmatchedHelperNotProbed(t *testing.T) {
	helpers := credentialHelpersOnPath(t, map[string]string{"fhecr": `touch "$FH_HELPER_DIR/ran"; exit 0`})
	t.Setenv("FH_HELPER_DIR", helpers)
	opCfg := operatorDockerConfig(t, `{"credHelpers":{"123456789012.dkr.ecr.us-east-1.amazonaws.com":"fhecr"}}`)
	var log strings.Builder
	st := containerStateEnv(t, "img:1", declSock, &log, map[string]string{gateDockerConfigEnvVar: opCfg})
	installGateState(t, st)
	g := scriptGateExec(t, nil)
	orderProbes(t, g)
	if out, code, disp := runDeclaredGate(t, context.Background(), t.TempDir()); code != 0 || disp != gateExecuted {
		t.Fatalf("exec = %d / %s: %s", code, disp, out)
	}
	if got := strings.Join(g.order, ","); got != "passwd,gate_run" {
		t.Errorf("calls = %s, want passwd,gate_run with no probe", got)
	}
	if _, err := os.Stat(filepath.Join(helpers, "ran")); !os.IsNotExist(err) {
		t.Errorf("the unmatched helper ran (%v)", err)
	}
	if strings.Contains(log.String(), "gate_credentials_probe") {
		t.Errorf("an unmatched helper was probed:\n%s", log.String())
	}
	if v, _ := envOf(g.env["gate_run"], "DOCKER_CONFIG"); v != opCfg {
		t.Errorf("gate_run DOCKER_CONFIG = %q, want the operator config %q", v, opCfg)
	}
	st.cleanup()
	if _, err := os.Stat(filepath.Join(opCfg, "config.json")); err != nil {
		t.Errorf("cleanup removed the operator's config: %v", err)
	}
	if strings.Contains(log.String(), "gate_docker_config_removed") {
		t.Errorf("cleanup logged removing an operator config:\n%s", log.String())
	}
}

// TestRunGateInContainer_AnsweringHelperProbedBeforeEveryExec: a matching
// helper that answers lets the exec proceed — the probe precedes the first
// runtime call and runs again on the next exec (no success caching) — and the
// gate image and the postgres service image are each probed.
func TestRunGateInContainer_AnsweringHelperProbedBeforeEveryExec(t *testing.T) {
	credentialHelpersOnPath(t, map[string]string{"fhok": `echo '{"Username":"u","Secret":"FH-CANARY-SECRET"}'`, "fhpg": "exit 1"})
	var log strings.Builder
	serviceContainerState(t, declSock, &log, map[string]string{
		gateImageEnvVar: "ghcr.io/o/gate:v1", gatePostgresImageEnvVar: "docker.io/library/postgres:16",
		gateDockerConfigEnvVar: operatorDockerConfig(t, `{"credsStore":"fhpg","credHelpers":{"ghcr.io":"fhok"}}`)})
	g := scriptGateExec(t, nil)
	orderProbes(t, g)
	for i := 0; i < 2; i++ {
		if out, code, _ := runDeclaredGate(t, context.Background(), t.TempDir()); code != 0 {
			t.Fatalf("exec %d exit %d: %s", i+1, code, out)
		}
	}
	// Each exec opens with both probes (the second exec's passwd read is a
	// cache hit, so its first runtime call is the service volume).
	execs := strings.Split(strings.Join(g.order, ","), "probe:docker-credential-fhok,probe:docker-credential-fhpg,")
	if len(execs) != 3 || execs[0] != "" || !strings.HasPrefix(execs[1], "passwd,") || !strings.HasPrefix(execs[2], "volume_create,") {
		t.Errorf("calls = %q, want both probes ahead of each exec's first runtime call", g.order)
	}
	for _, w := range []string{
		`"site":"gate","helper":"docker-credential-fhok","server":"ghcr.io","outcome":"answered"`,
		`"site":"gate","helper":"docker-credential-fhpg","server":"https://index.docker.io/v1/","outcome":"answered"`} {
		if n := strings.Count(log.String(), w); n != 2 {
			t.Errorf("%d lines %s, want 2:\n%s", n, w, log.String())
		}
	}
	if strings.Contains(log.String(), "FH-CANARY-SECRET") {
		t.Fatalf("a helper's answer reached the runner log:\n%s", log.String())
	}
}

// TestGateIsolationState_CleanupRemovesOwnedDockerConfigAfterCacheVolumes:
// cleanup removes the cache volumes WHILE the runner-owned docker config they
// were minted under still exists, THEN removes that dir. Swapping the two
// steps in cleanup turns the at-rm assertion red.
func TestGateIsolationState_CleanupRemovesOwnedDockerConfigAfterCacheVolumes(t *testing.T) {
	var log strings.Builder
	st := cacheState(t, &log, cacheRunA, cacheStageA, nil)
	g := scriptGateExec(t, nil)
	if _, code, _ := execOn(t, st); code != 0 {
		t.Fatalf("exit %d", code)
	}
	minted := st.dockerConfig.Dir
	if !st.dockerConfig.Owned() || g.counts["cache_create"] != 1 {
		t.Fatalf("fixture: config %+v, cache creates %d", st.dockerConfig, g.counts["cache_create"])
	}
	existedAtRm := false
	prev := execGateAuxArgvFn
	execGateAuxArgvFn = func(ctx context.Context, argv []string, dir string, env []string, d time.Duration) (string, int, bool) {
		if classifyGateArgv(argv) == "cache_rm" {
			cfg, _ := envOf(env, "DOCKER_CONFIG")
			_, err := os.Stat(filepath.Join(cfg, "config.json"))
			existedAtRm = cfg == minted && err == nil
		}
		return prev(ctx, argv, dir, env, d)
	}
	t.Cleanup(func() { execGateAuxArgvFn = prev })
	st.cleanup()
	if g.counts["cache_rm"] != 1 || !existedAtRm {
		t.Errorf("cache_rm = %d, config present at rm = %t; the volumes must go BEFORE the config dir", g.counts["cache_rm"], existedAtRm)
	}
	if _, err := os.Stat(minted); !os.IsNotExist(err) {
		t.Errorf("cleanup left the runner-owned docker config %s: %v", minted, err)
	}
	if !strings.Contains(log.String(), `"event":"gate_docker_config_removed","dir":"`+minted+`"`) {
		t.Errorf("missing gate_docker_config_removed:\n%s", log.String())
	}
	st.cleanup()
	if n := strings.Count(log.String(), "gate_docker_config_removed"); n != 1 {
		t.Errorf("a second cleanup re-removed (%d lines)", n)
	}
}

// TestGateIsolationState_DockerConfigRemovalFailureLogged: a failing removal
// is logged naming the dir and never panics.
func TestGateIsolationState_DockerConfigRemovalFailureLogged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	var log strings.Builder
	st := containerState("img:1", declSock, &log)
	cfg, err := st.gateDockerConfig()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(cfg.Dir)
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(locked, filepath.Base(cfg.Dir))
	if err := os.Rename(cfg.Dir, moved); err != nil {
		t.Skipf("cannot move the minted dir across %s: %v", parent, err)
	}
	st.dockerConfig.Dir = moved
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	st.cleanup()
	if !strings.Contains(log.String(), `"event":"gate_docker_config_cleanup_failed","dir":"`+moved+`"`) {
		t.Errorf("missing gate_docker_config_cleanup_failed:\n%s", log.String())
	}
}

// TestGateDockerConfig_NilStateRefused: a nil state has no cleanup to own a
// minted dir, so the container path refuses it gateUnavailable before any
// runtime call rather than leak a config dir.
func TestGateDockerConfig_NilStateRefused(t *testing.T) {
	installGateState(t, nil)
	g := scriptGateExec(t, nil)
	sel := containerState("img:1", declSock, io.Discard).selection(context.Background())
	out, code, disp := runGateInContainer(context.Background(), sel, []string{"true"}, t.TempDir(), filepath.Join(t.TempDir(), "lc"), nil, nil, time.Minute)
	if code != -1 || disp != gateUnavailable || !strings.HasPrefix(out, "gate container: docker config: no gate isolation state") {
		t.Fatalf("exec = %q / %d / %s", out, code, disp)
	}
	if len(g.order) != 0 {
		t.Errorf("runtime calls ran: %q", g.order)
	}
}

// TestGateIsolationState_SelectionStampsCredentials: the runner stamps the
// credential posture on a container-path selection only — anonymous by
// default, operator_config under the opt-in — and the gate_isolation_selected
// line and the recorded selection carry it; a refused or fallback selection
// carries none.
func TestGateIsolationState_SelectionStampsCredentials(t *testing.T) {
	opCfg := operatorDockerConfig(t, `{"auths":{}}`)
	rows := []struct {
		name string
		st   func(io.Writer) *gateIsolationState
		want gateiso.Credentials
	}{
		{"container anonymous", func(w io.Writer) *gateIsolationState { return containerState("img:1", declSock, w) }, gateiso.CredentialsAnonymous},
		{"container operator config", func(w io.Writer) *gateIsolationState {
			return containerStateEnv(t, "img:1", declSock, w, map[string]string{gateDockerConfigEnvVar: opCfg})
		}, gateiso.CredentialsOperatorConfig},
		{"refused", refusedState, ""},
		{"fallback", func(io.Writer) *gateIsolationState { return fallbackState() }, ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			st := r.st(&log)
			installGateState(t, st)
			captureHostExec(t, false, 0)
			scriptAuxExec(t, nil)
			stubSeed(t, func(string) error { return nil })
			_, _, _ = runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
			sel, ok := st.recordedSelection()
			if !ok || sel.Credentials != r.want {
				t.Fatalf("recorded credentials = %q (recorded %t), want %q", sel.Credentials, ok, r.want)
			}
			hasKey := strings.Contains(log.String(), `"credentials":"`+string(r.want)+`"}`)
			if r.want != "" && !hasKey {
				t.Errorf("gate_isolation_selected lacks credentials %q:\n%s", r.want, log.String())
			}
		})
	}
}

// TestDeclaredImage_PullFailureHintOnlyWhenAnonymous (#4046 approval
// condition 1): a pull failure under the anonymous posture names the
// FISHHAWK_GATE_DOCKER_CONFIG remedy after gateiso.PullFailedReason's own
// text; under an operator config (whose credentials were offered) it does not.
func TestDeclaredImage_PullFailureHintOnlyWhenAnonymous(t *testing.T) {
	const hint = "the runner pulls with a credential-free docker config (no credsStore, no credHelpers), so an image on a private registry needs FISHHAWK_GATE_DOCKER_CONFIG set to a docker config dir holding that registry's credentials"
	for _, r := range []struct {
		name     string
		extra    map[string]string
		wantHint bool
	}{
		{"anonymous", nil, true},
		{"operator config", map[string]string{gateDockerConfigEnvVar: operatorDockerConfig(t, `{"auths":{}}`)}, false},
	} {
		t.Run(r.name, func(t *testing.T) {
			declaredState(t, containerEnv(r.extra), safeDockerRuntime(declSock), stageImage("ghcr.io/o/private:v1"), nil)
			scriptGateExec(t, pulledImageResponder(1))
			out, code, disp := runDeclaredGate(t, context.Background(), t.TempDir())
			if code != -1 || disp != gateUnavailable || !strings.Contains(out, "could not be pulled from its registry") {
				t.Fatalf("exec = %q / %d / %s", out, code, disp)
			}
			if got := strings.Contains(out, hint); got != r.wantHint {
				t.Errorf("hint present = %t, want %t: %q", got, r.wantHint, out)
			}
			if r.wantHint && !strings.HasSuffix(out, "; "+hint) {
				t.Errorf("hint must follow PullFailedReason's text: %q", out)
			}
		})
	}
}

// TestRunGateInContainer_HostGoEnvNeverReachesContainer pins #4048 across the
// whole boundary: host-specific Go variables in the RUNNER PROCESS env (a
// toolchain-switching host exports a darwin GOROOT) go process env →
// sanitizedGateEnv → runGateInContainer → ContainerEnv → BuildArgv, and none of
// them may appear as a `-e` token on the gate `run` argv; the tuning knob
// survives, GOFLAGS loses its absolute-path field, and GOTOOLCHAIN stays the
// pin. The narrowing is container-only: the host-side module seed is handed the
// sanitized env UNCHANGED, GOROOT included, because it runs the host toolchain.
func TestRunGateInContainer_HostGoEnvNeverReachesContainer(t *testing.T) {
	hostRoot := "/Users/x/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.6.darwin-arm64"
	t.Setenv("GOROOT", hostRoot)
	t.Setenv("GOOS", "darwin")
	t.Setenv("GOWORK", "/Users/x/go.work")
	t.Setenv("GOTOOLDIR", "/Users/x/tool")
	t.Setenv("GODEBUG", "panicnil=1")
	t.Setenv("GOFLAGS", "-mod=mod -modfile=/Users/x/alt.mod")

	st := containerState("img:1", "/nonexistent/daemon.sock", &strings.Builder{})
	g := scriptGateExec(t, nil)
	// scriptGateExec stubs the seed itself, so the env-capturing seed goes in
	// AFTER it (and its cleanup restores the previous stub).
	var seedEnv []string
	prevSeed := seedModCacheFn
	seedModCacheFn = func(_ context.Context, _ gateiso.SeedExecFunc, _, _ string, _ *gateiso.VisibleCaches, baseEnv []string, _ time.Duration) (gateiso.SeedReport, error) {
		seedEnv = append([]string(nil), baseEnv...)
		return gateiso.SeedReport{}, nil
	}
	t.Cleanup(func() { seedModCacheFn = prevSeed })

	if out, code, disp := execOn(t, st); code != 0 || disp != gateExecuted {
		t.Fatalf("(out, code, disp) = (%q, %d, %s), want the gate's own verdict", out, code, disp)
	}
	gate := g.argv["gate_run"]
	if len(gate) == 0 {
		t.Fatalf("no gate run argv recorded: %v", g.order)
	}
	for _, key := range []string{"GOROOT", "GOOS", "GOWORK", "GOTOOLDIR"} {
		if i := envIndex(gate, key); i >= 0 {
			t.Errorf("host %s reached the container: -e %s", key, gate[i+1])
		}
	}
	if v, ok := lastEnvValue(gate, "GODEBUG"); !ok || v != "panicnil=1" {
		t.Errorf("GODEBUG = (%q, %v), want the tuning knob to survive verbatim", v, ok)
	}
	if v, ok := lastEnvValue(gate, "GOFLAGS"); !ok || v != "-mod=mod" {
		t.Errorf("GOFLAGS = (%q, %v), want the absolute -modfile field stripped to -mod=mod", v, ok)
	}
	if v, ok := lastEnvValue(gate, "GOTOOLCHAIN"); !ok || v != "local" {
		t.Errorf("GOTOOLCHAIN = (%q, %v), want the pin", v, ok)
	}
	if seedEnv == nil {
		t.Fatal("the host-side seed was never handed an env; the GOROOT check below would be vacuous")
	}
	if v, ok := envOf(seedEnv, "GOROOT"); !ok || v != hostRoot {
		t.Errorf("seed env GOROOT = (%q, %v), want the process GOROOT %q: the host seed needs the host toolchain", v, ok, hostRoot)
	}
}
