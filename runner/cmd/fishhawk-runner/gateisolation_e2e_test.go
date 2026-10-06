package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
)

// ---------------------------------------------------------------------------
// End-to-end gate-isolation fixtures (ADR-063 / #2134, plan step 12).
//
// These cross gateiso → the runner wiring (runBoundedGateArgv,
// runVerifyCommittedTree, runVerifyFixLoop) → a REAL container runtime and
// the pinned image. Every docker-gated fixture (a)–(g), (k)–(m), (o) skips with
// the detected reason when no safe runtime or the image is unavailable, and
// increments dockerFixturesRan at its END so main_test.go's TestMain
// sentinel (approval condition 3) turns an all-skipped run on a docker host
// into a failure. The fallback fixtures (h)–(j) need no runtime. Fixture (n)
// is OPT-IN (FISHHAWK_GATE_SELFHOST_IMAGE): it counts toward the sentinel
// only on a run that opted in and executed it.
// ---------------------------------------------------------------------------

// gateTestImageEnvVar overrides the pinned e2e image.
const gateTestImageEnvVar = "FISHHAWK_GATE_TEST_IMAGE"

// gateTestImageDefault is docker.io/alpine/git:v2.47.2 — git + busybox, with
// an ENTRYPOINT of git (which is exactly why BuildArgv resets it). Manifest
// verified 2026-09-15 (`docker manifest inspect`).
const gateTestImageDefault = "docker.io/alpine/git:v2.47.2"

// gateImageBinaries are the in-image binaries every fixture relies on;
// requireGateImage checks each one INSIDE the image and skips naming the
// first missing one rather than letting a fixture fail on a busybox gap.
// sh, mkdir and chown are the cache volume's docker prepare helper and
// install the rootless-podman one (#3967 approval condition 3): a gate image
// without them degrades every exec to the per-exec caches, so their absence
// is named here rather than only in a degrade log line.
var gateImageBinaries = []string{"git", "wget", "env", "sh", "cat", "ls", "sleep", "id", "stat", "test", "touch", "ln", "mkdir", "chown", "install"}

// gateImageCheck is the once-per-process outcome of requireGateImage.
type gateImageCheck struct {
	rt    gateiso.Runtime
	image string
	skip  string
}

var (
	gateImageOnce   sync.Once
	gateImageResult gateImageCheck
)

// requireGateImage resolves the runtime and image ONCE per process — the
// real gateiso.DetectRuntime over DefaultProbes (skip naming Runtime.Reason
// when unsafe or absent), then a `run --rm --network=none --entrypoint ”
// <image> /bin/sh -c '...'` that executes `sh -c 'echo ok'` (approval
// condition 1's live half: an ENTRYPOINT of git must not swallow the
// command) and proves every gateImageBinaries entry resolves in-image (a
// pull failure skips naming the runtime's error; a missing binary skips
// naming it). Every docker-gated fixture calls it first.
func requireGateImage(t *testing.T) (gateiso.Runtime, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short")
	}
	gateImageOnce.Do(func() {
		res := gateImageCheck{image: os.Getenv(gateTestImageEnvVar)}
		if res.image == "" {
			res.image = gateTestImageDefault
		}
		res.rt = gateiso.DetectRuntime(context.Background(), gateiso.DefaultProbes())
		if !res.rt.Safe {
			res.skip = "no safe container runtime: " + res.rt.Reason
			gateImageResult = res
			return
		}
		bin := res.rt.Kind.Binary()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if out, err := exec.CommandContext(ctx, bin, "pull", "-q", res.image).CombinedOutput(); err != nil {
			res.skip = fmt.Sprintf("image %s unavailable: %v: %s", res.image, err, strings.TrimSpace(string(out)))
			gateImageResult = res
			return
		}
		script := "sh -c 'echo ok' || { echo entrypoint-not-reset; exit 1; }; for b in " +
			strings.Join(gateImageBinaries, " ") +
			"; do command -v $b >/dev/null || { echo missing:$b; exit 1; }; done"
		argv := []string{bin, "run", "--rm", "--network=none", "--entrypoint", "", res.image, "/bin/sh", "-c", script}
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		text := strings.TrimSpace(string(out))
		switch {
		case err != nil && strings.Contains(text, "missing:"):
			i := strings.Index(text, "missing:")
			res.skip = fmt.Sprintf("image %s lacks binary %s", res.image, strings.Fields(text[i:])[0])
		case err != nil:
			res.skip = fmt.Sprintf("image %s binary check failed: %v: %s", res.image, err, text)
		case !strings.HasPrefix(text, "ok"):
			res.skip = fmt.Sprintf("image %s: sh -c 'echo ok' printed %q", res.image, text)
		}
		gateImageResult = res
	})
	if gateImageResult.skip != "" {
		t.Skip(gateImageResult.skip)
	}
	return gateImageResult.rt, gateImageResult.image
}

// liveContainerState installs a mode=container state bound to the REAL
// detected runtime and the pinned image (DefaultProbes, so the recorded
// selection carries the real endpoint) and returns it. It pins
// FISHHAWK_GATE_CACHE=off so fixtures (a)–(k) keep the per-exec caches —
// (k) plants into the per-exec cache dirs; fixture (o) owns process mode.
func liveContainerState(t *testing.T, rt gateiso.Runtime, image string) *gateIsolationState {
	t.Helper()
	st, err := configureGateIsolation(fakeEnv(map[string]string{
		gateIsolationModeEnvVar: "container", gateImageEnvVar: image, gateCacheEnvVar: "off"}), gateiso.DefaultProbes(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the once-per-process verdict rather than re-probing the daemon.
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return rt }
	installGateState(t, st)
	return st
}

// recordHostExec wraps the REAL host-exec seam with a recorder so a fixture
// can read the argv the runtime CLI was handed (the container name, the -v
// tokens) while the exec still happens.
func recordHostExec(t *testing.T) *[][]string {
	t.Helper()
	prev := execBoundedHostArgvFn
	var calls [][]string
	execBoundedHostArgvFn = func(ctx context.Context, argv []string, dir string, env []string, timeout time.Duration) (string, int, bool) {
		calls = append(calls, append([]string(nil), argv...))
		return prev(ctx, argv, dir, env, timeout)
	}
	t.Cleanup(func() { execBoundedHostArgvFn = prev })
	return &calls
}

// containerRunArgv returns the first `run` invocation recorded by
// recordHostExec (the subcommand follows the binary and its endpoint
// binding pair: `<bin> --host|--url unix://<socket> run …`).
func containerRunArgv(t *testing.T, calls [][]string) []string {
	t.Helper()
	for _, c := range calls {
		if len(c) > 4 && c[3] == "run" {
			return c
		}
	}
	t.Fatalf("no `run` argv recorded: %q", calls)
	return nil
}

// endpointBindingValue returns the value of the runtime argv's endpoint
// binding flag (`--host` for docker, `--url` for podman), "" when absent.
func endpointBindingValue(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--host" || argv[i] == "--url" {
			return argv[i+1]
		}
	}
	return ""
}

// runtimeArgvBeforeImage returns the runtime-side tokens of a BuildArgv line:
// everything up to and including the `--entrypoint ""` pair, excluding the
// image and the gate argv that follow it.
func runtimeArgvBeforeImage(t *testing.T, argv []string) []string {
	t.Helper()
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--entrypoint" && argv[i+1] == "" {
			return argv[:i+2]
		}
	}
	t.Fatalf("no --entrypoint \"\" pair in argv %q", argv)
	return nil
}

// argvMountSource returns the host side of the -v token mounted at target.
func argvMountSource(argv []string, target string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-v" {
			if src, ok := strings.CutSuffix(argv[i+1], ":"+target); ok {
				return src
			}
		}
	}
	return ""
}

// primaryWithLinkedWorktree builds a primary repo with one commit, an
// origin/main ref, and a LINKED worktree — the shape the runner's own working
// directory has under a run — and returns (primary, linked, head).
func primaryWithLinkedWorktree(t *testing.T) (string, string, string) {
	t.Helper()
	repo, head := gateRepoWithCommit(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", linked, head).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", linked).Run() })
	return repo, linked, head
}

// primaryUntouched asserts the primary carries neither the planted ref nor
// the planted hook a gate wrote into its throwaway checkout.
func primaryUntouched(t *testing.T, primary string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", primary, "show-ref", "--verify", "--quiet", "refs/heads/planted").CombinedOutput(); err == nil {
		t.Errorf("refs/heads/planted reached the primary: %s", out)
	}
	if _, err := os.Stat(filepath.Join(primary, ".git", "hooks", "pre-commit")); err == nil {
		t.Errorf("the planted pre-commit hook reached the primary")
	}
}

// plantCmd is the gate command every .git-isolation fixture runs: plant a
// hook and a ref in the checkout it was handed. It prints the shell-visible
// facts the fixtures assert on and exits 0 so the outcome is "passed".
func plantCmd(primaryAbs string) string {
	return strings.Join([]string{
		`printf 'gitdir=%s\n' "$(git rev-parse --path-format=absolute --git-common-dir)"`,
		`printf 'lock=%s\n' "$FISHHAWK_VERIFY_LOCK_PATH"`,
		`if ls ` + primaryAbs + ` >/dev/null 2>&1; then echo primary=visible; else echo primary=unreachable; fi`,
		`mkdir -p "$(git rev-parse --git-dir)/hooks" && printf '#!/bin/sh\nexit 1\n' > "$(git rev-parse --git-dir)/hooks/pre-commit"`,
		`git update-ref refs/heads/planted HEAD && echo planted=ok`,
	}, "; ")
}

func outputField(out, key string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), key+"="); ok {
			return v
		}
	}
	return ""
}

// listenLoopback serves HTTP on 127.0.0.1 (every request answered 200 `ok`),
// returning its port: the host-loopback target the no-network fixtures must
// FAIL to reach. It ANSWERS rather than accept-and-drop (#3448 note 3) so a
// host-side positive control can prove the listener is live — a container
// `loopback=failed` then discriminates on --network=none, not on a dead
// listener that would fail the fetch from anywhere.
func listenLoopback(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(l) }()
	return l.Addr().(*net.TCPAddr).Port
}

// requireLoopbackAnswers is the host-side positive control for listenLoopback:
// an http.Get from the test process must return 200 before a container is
// asked to reach the same port. It fails (never skips) on any transport error
// or non-200, so a dead listener cannot green the container's
// `loopback=failed` assertion.
func requireLoopbackAnswers(t *testing.T, port int) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("positive control: host-side GET of the loopback listener on 127.0.0.1:%d failed: %v", port, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("positive control: host-side GET of 127.0.0.1:%d returned %d, want 200", port, resp.StatusCode)
	}
}

// (a) TestGateContainer_PrimaryGitUnreachable: on the container path the
// verify gate's throwaway checkout is an independent clone mounted at /work
// (its git common dir is INSIDE /work), the primary's absolute path is
// unreachable from the container, a planted hook and a planted ref never
// reach the primary, and FISHHAWK_VERIFY_LOCK_PATH is NOT injected (the
// primary's lock file is not visible there).
func TestGateContainer_PrimaryGitUnreachable(t *testing.T) {
	rt, image := requireGateImage(t)
	primary, linked, head := primaryWithLinkedWorktree(t)
	liveContainerState(t, rt, image)
	primaryAbs, _ := filepath.EvalSymlinks(primary)
	_, out, outcome, _ := runVerifyCommittedTree(context.Background(), plantCmd(primaryAbs), linked, head, 3*time.Minute, nil)
	if outcome != "passed" {
		t.Fatalf("outcome %q:\n%s", outcome, out)
	}
	if got := outputField(out, "gitdir"); !strings.HasPrefix(got, gateiso.MountWork+"/") {
		t.Errorf("clone git common dir = %q, want under %s (an independent .git inside the mount)", got, gateiso.MountWork)
	}
	if got := outputField(out, "primary"); got != "unreachable" {
		t.Errorf("primary %s is %q from the container, want unreachable", primaryAbs, got)
	}
	if got := outputField(out, "lock"); got != "" {
		t.Errorf("FISHHAWK_VERIFY_LOCK_PATH=%q crossed into the container; the primary's lock is unreachable there", got)
	}
	if outputField(out, "planted") != "ok" {
		t.Errorf("planting inside the clone failed:\n%s", out)
	}
	primaryUntouched(t, primary)
	dockerFixturesRan++
}

// (b) TestGateContainer_NoNetwork: an external fetch and a fetch of a LIVE
// host-loopback listener both fail, and no ethernet interface is present.
// The listener is proven live from the HOST first (requireLoopbackAnswers,
// #3448 note 3), so `loopback=failed` inside the container discriminates on
// --network=none rather than on a dead listener. (/sys/class/net is asserted
// for the ABSENCE of eth*, not for "only lo": Docker Desktop's VM kernel
// lists tunnel pseudo-devices such as gre0 in every network namespace.)
func TestGateContainer_NoNetwork(t *testing.T) {
	rt, image := requireGateImage(t)
	port := listenLoopback(t)
	requireLoopbackAnswers(t, port)
	liveContainerState(t, rt, image)
	cmd := fmt.Sprintf(`wget -q -T 3 -O /dev/null http://example.com/ && echo external=reached || echo external=failed; `+
		`wget -q -T 3 -O /dev/null http://127.0.0.1:%d/ && echo loopback=reached || echo loopback=failed; `+
		`printf 'ifaces=%%s\n' "$(ls /sys/class/net | tr '\n' ' ')"`, port)
	out, code := runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if outputField(out, "external") != "failed" {
		t.Errorf("external network reachable from the gate container:\n%s", out)
	}
	if outputField(out, "loopback") != "failed" {
		t.Errorf("host loopback listener on 127.0.0.1:%d reachable from the gate container:\n%s", port, out)
	}
	ifaces := strings.Fields(outputField(out, "ifaces"))
	var hasLo bool
	for _, i := range ifaces {
		if i == "lo" {
			hasLo = true
		}
		if strings.HasPrefix(i, "eth") || strings.HasPrefix(i, "en") {
			t.Errorf("network interface %q present under --network=none (ifaces=%v)", i, ifaces)
		}
	}
	if !hasLo {
		t.Errorf("lo missing from /sys/class/net (ifaces=%v)", ifaces)
	}
	dockerFixturesRan++
}

// (c) TestGateContainer_HostFSUnreadable: a marker OUTSIDE the four mounts is
// unreadable from the container, while the fallback (host-exec) control
// reads it — the discrimination that proves the assertion is about the
// container, not about the marker.
func TestGateContainer_HostFSUnreadable(t *testing.T) {
	rt, image := requireGateImage(t)
	marker := filepath.Join(t.TempDir(), "host-marker")
	mustWrite(t, marker, "host-secret\n")
	cmd := `if cat ` + marker + ` >/dev/null 2>&1; then echo marker=readable; else echo marker=unreadable; fi`

	installGateState(t, nil) // fallback control: host exec sees the host filesystem
	out, code := runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
	if code != 0 || outputField(out, "marker") != "readable" {
		t.Fatalf("host-exec control: exit %d marker=%q (want readable):\n%s", code, outputField(out, "marker"), out)
	}

	liveContainerState(t, rt, image)
	out, code = runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if outputField(out, "marker") != "unreadable" {
		t.Errorf("host marker %s readable from the gate container:\n%s", marker, out)
	}
	dockerFixturesRan++
}

// (d) TestGateContainer_NoDaemonSocket: no docker/podman socket node exists
// inside the container (at the conventional paths or anywhere under the four
// mounts), the captured runtime argv carries no socket token, and a checkout
// carrying a planted unix socket is REFUSED before the runtime CLI is
// reached — with the REAL seam recorded, not a fake.
func TestGateContainer_NoDaemonSocket(t *testing.T) {
	rt, image := requireGateImage(t)
	liveContainerState(t, rt, image)
	calls := recordHostExec(t)
	cmd := `for s in /var/run/docker.sock /run/docker.sock /run/podman/podman.sock; do test -e $s && echo socket=$s; done; ` +
		`found=$(find /work /gocache /gomodcache /lintcache /run /var/run -type s 2>/dev/null | head -1); printf 'mountsocket=%s\n' "$found"; echo done=ok`
	out, code := runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 || outputField(out, "done") != "ok" {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := outputField(out, "socket"); got != "" {
		t.Errorf("daemon socket %s present inside the gate container", got)
	}
	if got := outputField(out, "mountsocket"); got != "" {
		t.Errorf("unix socket %s visible inside the gate container", got)
	}
	// Only the RUNTIME-side tokens (everything before the image) are scanned:
	// the gate command after the image is this fixture's own text and names
	// the socket paths it probes for. The ONE legitimate socket token is the
	// endpoint binding value (`--host`/`--url unix://<validated socket>`),
	// which tells the CLI where to CONNECT; it is asserted equal to the
	// validated socket and excluded from the mount scan — no other token,
	// and no -v source, may name a socket.
	run := containerRunArgv(t, *calls)
	if got, want := endpointBindingValue(run), "unix://"+rt.SocketPath; got != want {
		t.Errorf("endpoint binding = %q, want %q (the validated socket)", got, want)
	}
	runtimeSide := runtimeArgvBeforeImage(t, run)
	for i, tok := range runtimeSide {
		if i > 0 && (runtimeSide[i-1] == "--host" || runtimeSide[i-1] == "--url") {
			continue
		}
		if strings.Contains(tok, "docker.sock") || strings.Contains(tok, "podman.sock") || strings.HasPrefix(tok, "/var/run") || strings.HasPrefix(tok, "/run/") {
			t.Errorf("runtime argv carries a socket token %q: %q", tok, run)
		}
		if rt.SocketPath != "" && strings.Contains(tok, rt.SocketPath) {
			t.Errorf("runtime argv names the daemon socket %s: %q", rt.SocketPath, run)
		}
	}

	// Planted socket in the checkout: refused before the seam. A SHORT path,
	// since unix socket paths are capped near 104 bytes on macOS.
	dir, err := os.MkdirTemp("/tmp", "fh-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatalf("unix socket unavailable: %v", err)
	}
	defer l.Close()
	before := len(*calls)
	out, code = runBoundedGateCommand(context.Background(), "true", dir, filepath.Join(t.TempDir(), "lc"), time.Minute)
	if code != -1 || !strings.Contains(out, "refused") || !strings.Contains(out, "unix socket") {
		t.Errorf("planted-socket checkout: exit %d out %q, want -1 with the mount-guard refusal", code, out)
	}
	if len(*calls) != before {
		t.Errorf("runtime CLI reached despite the planted socket: %q", (*calls)[before:])
	}
	dockerFixturesRan++
}

// (l) TestGateContainer_EndpointBoundAcrossContextSwitch: the endpoint the
// selection validated is the one a LATER gate connects to, even after the
// runtime configuration changes underneath it. The selection is recorded
// against the real detected socket, then DOCKER_HOST / CONTAINER_HOST are
// redirected to an unreachable tcp endpoint and DOCKER_CONTEXT /
// CONTAINER_CONNECTION to a nonexistent context — the `docker context use`
// shape between two gates — and a gate still executes in a container on the
// validated daemon (exit 0, `ok` printed), the recorded argv opens with the
// binding, and the env the CLI received pins the validated socket with the
// redirecting variables dropped. Deleting the binding turns this red: the
// CLI follows the redirected endpoint and the gate never runs.
func TestGateContainer_EndpointBoundAcrossContextSwitch(t *testing.T) {
	rt, image := requireGateImage(t)
	st := liveContainerState(t, rt, image)
	if sel := st.selection(context.Background()); sel.Runtime.SocketPath != rt.SocketPath {
		t.Fatalf("selection socket %q != detected %q", sel.Runtime.SocketPath, rt.SocketPath)
	}
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	t.Setenv("CONTAINER_HOST", "tcp://127.0.0.1:1")
	t.Setenv("DOCKER_CONTEXT", "fishhawk-nonexistent-context")
	t.Setenv("CONTAINER_CONNECTION", "fishhawk-nonexistent-connection")
	var envs [][]string
	prev := execBoundedHostArgvFn
	execBoundedHostArgvFn = func(ctx context.Context, argv []string, dir string, env []string, timeout time.Duration) (string, int, bool) {
		envs = append(envs, append([]string(nil), env...))
		return prev(ctx, argv, dir, env, timeout)
	}
	t.Cleanup(func() { execBoundedHostArgvFn = prev })
	calls := recordHostExec(t)
	out, code := runBoundedGateCommand(context.Background(), "echo ok", t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("gate under a redirected runtime configuration: exit %d out %q; want 0/ok on the validated daemon", code, out)
	}
	run := containerRunArgv(t, *calls)
	if got, want := endpointBindingValue(run), "unix://"+rt.SocketPath; got != want {
		t.Errorf("endpoint binding = %q, want %q", got, want)
	}
	if len(envs) == 0 {
		t.Fatal("no env recorded")
	}
	pin := "DOCKER_HOST=unix://" + rt.SocketPath
	if rt.Kind == gateiso.KindPodman {
		pin = "CONTAINER_HOST=unix://" + rt.SocketPath
	}
	found := false
	for _, kv := range envs[0] {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case kv == pin:
			found = true
		case k == "DOCKER_CONTEXT", k == "CONTAINER_CONNECTION", kv == "DOCKER_HOST=tcp://127.0.0.1:1", kv == "CONTAINER_HOST=tcp://127.0.0.1:1":
			t.Errorf("redirecting variable reached the runtime CLI: %s", kv)
		}
	}
	if !found {
		t.Errorf("runtime CLI env lacks %s", pin)
	}
	dockerFixturesRan++
}

// (e) TestGateContainer_EnvAllowList: runner credentials set in the RUNNER's
// environment never reach the container, GOPROXY=off is pinned, and a
// caller-supplied extraEnv entry is present (env preservation across the
// container boundary).
func TestGateContainer_EnvAllowList(t *testing.T) {
	rt, image := requireGateImage(t)
	canary := "e2e-canary-" + strconv.Itoa(os.Getpid())
	for _, k := range []string{"FISHHAWK_GITHUB_TOKEN", "ANTHROPIC_API_KEY", "FISHHAWK_API_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(k, canary)
	}
	t.Setenv("GOFLAGS", "-mod=mod") // allow-listed GO* survives
	liveContainerState(t, rt, image)
	out, code := runBoundedGateCommand(context.Background(), "env", t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute, "FISHHAWK_E2E_EXTRA=present")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, canary) {
		t.Errorf("a runner credential crossed into the container:\n%s", out)
	}
	for _, k := range []string{"FISHHAWK_GITHUB_TOKEN=", "ANTHROPIC_API_KEY=", "FISHHAWK_API_TOKEN=", "GITHUB_TOKEN="} {
		if strings.Contains(out, "\n"+k) || strings.HasPrefix(out, k) {
			t.Errorf("%s present in the container env:\n%s", k, out)
		}
	}
	for _, want := range []string{"GOPROXY=off", "GOTOOLCHAIN=local", "GOMODCACHE=" + gateiso.MountGoModCache, "GOCACHE=" + gateiso.MountGoCache,
		"GOLANGCI_LINT_CACHE=" + gateiso.MountLintCache, "FISHHAWK_E2E_EXTRA=present", "GOFLAGS=-mod=mod", "GIT_CONFIG_GLOBAL=/dev/null"} {
		if !strings.Contains(out, want+"\n") && !strings.HasSuffix(out, want) {
			t.Errorf("container env lacks %s:\n%s", want, out)
		}
	}
	dockerFixturesRan++
}

// (f) TestGateContainer_TimeoutKillsContainer: a gate that outlives its
// timeout returns -1 within a scaled bound AND the container is gone —
// killing the runtime CLI does not stop the container, so this is red when
// runGateInContainer's KillArgv step is deleted (`ps -a` still lists it).
func TestGateContainer_TimeoutKillsContainer(t *testing.T) {
	rt, image := requireGateImage(t)
	liveContainerState(t, rt, image)
	calls := recordHostExec(t)
	timeout := scaledD(2 * time.Second)
	bound := scaledD(30 * time.Second)
	start := time.Now()
	out, code := runBoundedGateCommand(context.Background(), "sleep 60", t.TempDir(), filepath.Join(t.TempDir(), "lc"), timeout)
	elapsed := time.Since(start)
	run := containerRunArgv(t, *calls)
	name := run[4]
	t.Cleanup(func() { _ = exec.Command(rt.Kind.Binary(), "rm", "-f", name).Run() })
	if code != -1 {
		t.Fatalf("exit %d, want -1 (timeout): %s", code, out)
	}
	if elapsed > bound {
		t.Errorf("timed-out gate returned after %s, bound %s", elapsed, bound)
	}
	ps, err := exec.Command(rt.Kind.Binary(), "ps", "-a", "--filter", "name="+name, "--format", "{{.Names}}").CombinedOutput()
	if err != nil {
		t.Fatalf("%s ps: %v: %s", rt.Kind.Binary(), err, ps)
	}
	if strings.TrimSpace(string(ps)) != "" {
		t.Errorf("container %s still present after the timeout:\n%s", name, ps)
	}
	dockerFixturesRan++
}

// (g) TestGateContainer_LinuxOwnership: the container runs as the runner's
// uid, and on Linux a file it writes into the mounted checkout is owned by
// os.Getuid() on the host (--user uid:gid / --userns=keep-id).
func TestGateContainer_LinuxOwnership(t *testing.T) {
	rt, image := requireGateImage(t)
	liveContainerState(t, rt, image)
	dir := t.TempDir()
	out, code := runBoundedGateCommand(context.Background(), `printf 'uid=%s\n' "$(id -u)"; touch written`, dir, filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := outputField(out, "uid"); got != strconv.Itoa(os.Getuid()) {
		t.Errorf("container uid = %q, want the runner's %d", got, os.Getuid())
	}
	st, err := os.Stat(filepath.Join(dir, "written"))
	if err != nil {
		t.Fatalf("file written in /work not visible on the host: %v", err)
	}
	if runtime.GOOS == "linux" {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
			t.Errorf("host owner of the written file = %d, want %d", sys.Uid, os.Getuid())
		}
	}
	dockerFixturesRan++
}

// (k) TestGateContainer_CacheSymlinkNeverReachesHost: a gate that plants
// symlinks in /gomodcache (one to a read-only host canary outside every
// mount, one to a directory) leaves nothing behind — the per-exec visible
// cache dir is gone after the exec, the canary's mode and contents are
// untouched (symlink-safe cleanup, approval condition 2), and the host
// GOMODCACHE carries no planted entry.
func TestGateContainer_CacheSymlinkNeverReachesHost(t *testing.T) {
	rt, image := requireGateImage(t)
	canary := filepath.Join(t.TempDir(), "canary")
	mustWrite(t, canary, "canary\n")
	if err := os.Chmod(canary, 0o444); err != nil {
		t.Fatal(err)
	}
	hostModCache := hostGoModCache(t)
	liveContainerState(t, rt, image)
	calls := recordHostExec(t)
	cmd := `ln -s ` + canary + ` /gomodcache/planted && ln -s /work /gomodcache/planted-dir && mkdir -p /gomodcache/cache/download && touch /gomodcache/cache/download/planted-file && echo planted=ok`
	out, code := runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 || outputField(out, "planted") != "ok" {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	run := containerRunArgv(t, *calls)
	visible := argvMountSource(run, gateiso.MountGoModCache)
	if visible == "" {
		t.Fatalf("no %s mount in argv %q", gateiso.MountGoModCache, run)
	}
	if visible == hostModCache || strings.HasPrefix(visible, hostModCache+string(filepath.Separator)) {
		t.Fatalf("the HOST module cache %s was mounted into the container (%s)", hostModCache, visible)
	}
	if _, err := os.Lstat(visible); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("visible cache dir %s survives the exec (err=%v)", visible, err)
	}
	if _, err := os.Lstat(filepath.Dir(visible)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("visible cache root %s survives the exec (err=%v)", filepath.Dir(visible), err)
	}
	st, err := os.Stat(canary)
	if err != nil {
		t.Fatalf("canary: %v", err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Errorf("canary mode = %o, want 0444 (cleanup chmod'd THROUGH the planted symlink)", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(canary); string(b) != "canary\n" {
		t.Errorf("canary contents changed: %q", b)
	}
	for _, p := range []string{filepath.Join(hostModCache, "planted"), filepath.Join(hostModCache, "planted-dir"), filepath.Join(hostModCache, "cache", "download", "planted-file")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("planted entry reached the host module cache: %s", p)
		}
	}
	dockerFixturesRan++
}

// hostGoModCache resolves the host's GOMODCACHE (`go env`), skipping when go
// is absent.
func hostGoModCache(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	out, err := exec.Command(goBin, "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOMODCACHE: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// (h) TestGateClone_PlantedRefNeverReachesPrimary: on the clone (host-exec)
// path the verify gate's checkout is an independent clone, so a planted hook
// and ref never reach the primary — and the primary's lock path IS injected.
// The sibling `git worktree add --detach` checkout proves the discrimination:
// under the pre-#2134 materialization the ref DOES reach the primary.
func TestGateClone_PlantedRefNeverReachesPrimary(t *testing.T) {
	st, err := configureGateIsolation(fakeEnv(map[string]string{gateIsolationModeEnvVar: "clone"}), gateiso.Probes{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return gateiso.Runtime{Kind: gateiso.KindNone} }
	st.probeSandbox = func(context.Context) (bool, string) { return false, "none" }
	installGateState(t, st)
	if sel := st.selection(context.Background()); sel.Path != gateiso.PathClone {
		t.Fatalf("selection = %+v, want clone", sel)
	}
	primary, linked, head := primaryWithLinkedWorktree(t)
	primaryAbs, _ := filepath.EvalSymlinks(primary)
	_, out, outcome, _ := runVerifyCommittedTree(context.Background(), plantCmd(primaryAbs), linked, head, time.Minute, nil)
	if outcome != "passed" || outputField(out, "planted") != "ok" {
		t.Fatalf("outcome %q:\n%s", outcome, out)
	}
	if got := outputField(out, "gitdir"); got == primaryAbs+"/.git" || got == filepath.Join(primary, ".git") || !strings.Contains(got, "fishhawk-verify-") {
		t.Errorf("clone git common dir = %q, want the throwaway clone's own .git", got)
	}
	if got := outputField(out, "lock"); filepath.Base(got) != verifyLockFileName {
		t.Errorf("FISHHAWK_VERIFY_LOCK_PATH = %q, want the primary's %s on the clone path", got, verifyLockFileName)
	}
	primaryUntouched(t, primary)

	// Discrimination sibling: a linked worktree (the pre-#2134 materialization)
	// shares refs with the primary, so the same plant DOES reach it.
	sibling := filepath.Join(t.TempDir(), "sibling")
	if out, err := exec.Command("git", "-C", primary, "worktree", "add", "--detach", sibling, head).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("git", "-C", primary, "worktree", "remove", "--force", sibling).Run() })
	if out, err := exec.Command("git", "-C", sibling, "update-ref", "refs/heads/planted-sibling", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("update-ref in the sibling worktree: %v\n%s", err, out)
	}
	if err := exec.Command("git", "-C", primary, "show-ref", "--verify", "--quiet", "refs/heads/planted-sibling").Run(); err != nil {
		t.Errorf("control: a ref planted through a linked worktree must reach the primary (the discrimination the clone fixture depends on)")
	}
}

// (i) TestGateCloneSandbox_NoNetwork: Linux-only — under the clone-sandbox
// path a connect to a LIVE host-loopback listener fails through the runner's
// own runBoundedGateCommand, while the same connect succeeds under the clone
// path (the control that proves the listener is reachable at all).
func TestGateCloneSandbox_NoNetwork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("clone-sandbox is Linux-only (unshare -rn); GOOS=%s", runtime.GOOS)
	}
	if avail, reason := gateiso.ProbeSandbox(context.Background()); !avail {
		t.Skipf("clone-sandbox unavailable: %s", reason)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	port := listenLoopback(t)
	cmd := fmt.Sprintf(`python3 -c "import socket; socket.create_connection(('127.0.0.1', %d), timeout=2)" && echo connect=ok || echo connect=failed`, port)

	installGateState(t, nil) // control: host exec reaches the listener
	out, _ := runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
	if outputField(out, "connect") != "ok" {
		t.Fatalf("control: host exec could not reach 127.0.0.1:%d:\n%s", port, out)
	}

	st, err := configureGateIsolation(fakeEnv(map[string]string{gateIsolationModeEnvVar: "clone-sandbox"}), gateiso.Probes{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return gateiso.Runtime{Kind: gateiso.KindNone} }
	installGateState(t, st)
	if sel := st.selection(context.Background()); sel.Path != gateiso.PathCloneSandbox {
		t.Fatalf("selection = %+v, want clone-sandbox", sel)
	}
	out, _ = runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
	if outputField(out, "connect") != "failed" {
		t.Errorf("host loopback reachable under clone-sandbox:\n%s", out)
	}
}

// (j) TestGateHosted_RefusesEndToEnd: a hosted profile with mode auto whose
// runtime is REALLY detected (DefaultProbes) but whose effective endpoint is
// remote (DOCKER_HOST / CONTAINER_HOST pinned to tcp:// through the Getenv
// probe, so the verdict comes from gateiso.DetectRuntime's classification,
// not a canned Runtime) is refused end to end: the single-shot gate wraps
// ErrVerifyInfraFailure (category C), the fix loop breaks with category C
// and never invokes the fix agent, and the verify command never runs.
func TestGateHosted_RefusesEndToEnd(t *testing.T) {
	probes := gateiso.DefaultProbes()
	probes.Getenv = func(k string) string {
		switch k {
		case "DOCKER_HOST", "CONTAINER_HOST":
			return "tcp://10.0.0.5:2376"
		}
		return os.Getenv(k)
	}
	st, err := configureGateIsolation(fakeEnv(map[string]string{deploymentProfileEnvVar: "hosted"}), probes, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	installGateState(t, st)
	sel := st.selection(context.Background())
	if !sel.Refused() || sel.Runtime.Safe {
		t.Fatalf("selection = %+v, want refused with an unsafe runtime", sel)
	}
	if _, err := exec.LookPath("docker"); err == nil && !strings.Contains(sel.Runtime.Reason, "tcp://10.0.0.5:2376") {
		t.Errorf("runtime reason %q does not name the remote endpoint", sel.Runtime.Reason)
	}

	repo, _, _ := verifiedTreeRepo(t)
	var log strings.Builder
	events, tree, err := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), &log)
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) || !errors.Is(err, errGateIsolationRefused) || tree != "" {
		t.Fatalf("single-shot gate: err=%v tree=%q, want category C refusal", err, tree)
	}
	var failedRuns int
	for _, ev := range events {
		if ev.Kind != "verify_run" {
			continue
		}
		var p struct {
			Output  string `json:"output"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("verify_run payload: %v", err)
		}
		if p.Outcome == "failed" && strings.HasPrefix(p.Output, gateIsolationRefusedSignature) {
			failedRuns++
		}
	}
	if failedRuns != 1 {
		t.Errorf("verify_run failed-with-signature events = %d, want 1: %+v", failedRuns, events)
	}

	cfg, logPath := verifyFixLoopScopeFixture(t, verifyScopeGoFiles, 0, 0)
	res := agent.Result{OK: true}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	log.Reset()
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", invoker, agent.Invocation{}, &res, &log)
	if err != nil || reinvoked || tree != "" {
		t.Fatalf("fix loop: err=%v reinvoked=%t tree=%q", err, reinvoked, tree)
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
	if !strings.Contains(log.String(), `"event":"verify_gate_refused"`) {
		t.Errorf("log lacks verify_gate_refused:\n%s", log.String())
	}
}

// ---------------------------------------------------------------------------
// Gate services (ADR-063 amendment gap 1, E51.4 / #2137): fixtures (m), (n).
// ---------------------------------------------------------------------------

// gateSelfHostImageEnvVar opts fixture (n) in: the built fishhawk-gate image
// (deploy/gate-image), which carries the Go toolchain the pgtest suite needs.
const gateSelfHostImageEnvVar = "FISHHAWK_GATE_SELFHOST_IMAGE"

var (
	gatePgImageOnce sync.Once
	gatePgImageSkip string
)

// requirePostgresImage makes the Postgres service image available ONCE per
// process (a pull, falling back to an image already present locally) and
// proves the in-image binaries fixture (m) relies on — it runs the SAME image
// as the gate image, for its psql client and busybox. Skips naming the error.
func requirePostgresImage(t *testing.T, rt gateiso.Runtime) string {
	t.Helper()
	image := gateiso.DefaultPostgresImage
	gatePgImageOnce.Do(func() {
		bin := rt.Kind.Binary()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if out, err := exec.CommandContext(ctx, bin, "pull", "-q", image).CombinedOutput(); err != nil {
			if exec.CommandContext(ctx, bin, "image", "inspect", image).Run() != nil {
				gatePgImageSkip = fmt.Sprintf("image %s unavailable: %v: %s", image, err, strings.TrimSpace(string(out)))
				return
			}
		}
		script := "for b in psql wget id touch ls env tr find; do command -v $b >/dev/null || { echo missing:$b; exit 1; }; done; echo ok"
		out, err := exec.CommandContext(ctx, bin, "run", "--rm", "--network=none", "--entrypoint", "", image, "/bin/sh", "-c", script).CombinedOutput()
		if text := strings.TrimSpace(string(out)); err != nil || !strings.HasPrefix(text, "ok") {
			gatePgImageSkip = fmt.Sprintf("image %s binary check failed: %v: %s", image, err, text)
		}
	})
	if gatePgImageSkip != "" {
		t.Skip(gatePgImageSkip)
	}
	return image
}

// liveServiceState installs a mode=container state bound to the REAL detected
// runtime with FISHHAWK_GATE_SERVICES=services and the given gate and
// Postgres images, logging to logSink (cache off, like liveContainerState).
func liveServiceState(t *testing.T, rt gateiso.Runtime, image, services string, logSink io.Writer) *gateIsolationState {
	t.Helper()
	st, err := configureGateIsolation(fakeEnv(map[string]string{
		gateIsolationModeEnvVar: "container", gateImageEnvVar: image, gateCacheEnvVar: "off",
		gateServicesEnvVar: services, gatePostgresImageEnvVar: gateiso.DefaultPostgresImage}), gateiso.DefaultProbes(), logSink)
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return rt }
	installGateState(t, st)
	return st
}

// recordAuxExec wraps the REAL gate-service/passwd seam with a recorder.
func recordAuxExec(t *testing.T) *[][]string {
	t.Helper()
	prev := execGateAuxArgvFn
	var calls [][]string
	execGateAuxArgvFn = func(ctx context.Context, argv []string, dir string, env []string, timeout time.Duration) (string, int, bool) {
		calls = append(calls, append([]string(nil), argv...))
		return prev(ctx, argv, dir, env, timeout)
	}
	t.Cleanup(func() { execGateAuxArgvFn = prev })
	return &calls
}

// runtimeHostCmd runs `<bin> <endpoint binding> args…` on the HOST under an
// env bound to the validated endpoint — the inspection side of the fixtures.
func runtimeHostCmd(t *testing.T, rt gateiso.Runtime, args ...string) (string, error) {
	t.Helper()
	endpoint, err := rt.EndpointArgs()
	if err != nil {
		t.Fatalf("endpoint args: %v", err)
	}
	env, err := rt.BindEndpointEnv(os.Environ())
	if err != nil {
		t.Fatalf("bind endpoint env: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, rt.Kind.Binary(), append(endpoint, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// serviceInspect is the subset of `<runtime> inspect` fixture (m) asserts.
type serviceInspect struct {
	Config struct {
		User   string
		Env    []string
		Labels map[string]string
	}
	HostConfig struct {
		NetworkMode     string
		CapDrop         []string
		SecurityOpt     []string
		Privileged      bool
		PublishAllPorts bool
		PortBindings    map[string]any
		Tmpfs           map[string]string
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
	}
}

// inspectGateService is fixture (m)'s gateServiceObserver body: it inspects
// the provisioned service on the host BEFORE the gate exec and returns the
// superuser password from the service env (so the fixture can prove it never
// reached the gate). Every containment property is asserted here: no network,
// all capabilities dropped, no-new-privileges, the non-root user, no published
// port, PGDATA on tmpfs, and EXACTLY ONE mount — the named socket volume — so
// no bind mount and no anonymous data volume (#2137 approval condition 2).
func inspectGateService(t *testing.T, ctx context.Context, rt gateiso.Runtime, cliEnv []string, svc gateiso.PostgresService) string {
	t.Helper()
	endpoint, err := rt.EndpointArgs()
	if err != nil {
		t.Errorf("observer: endpoint args: %v", err)
		return ""
	}
	cmd := exec.CommandContext(ctx, rt.Kind.Binary(), append(endpoint, "inspect", svc.Name)...)
	cmd.Env = cliEnv
	raw, err := cmd.Output()
	if err != nil {
		t.Errorf("observer: inspect %s: %v", svc.Name, err)
		return ""
	}
	var got []serviceInspect
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != 1 {
		t.Errorf("observer: inspect %s: %v (%d objects)", svc.Name, err, len(got))
		return ""
	}
	in := got[0]
	if in.HostConfig.NetworkMode != "none" {
		t.Errorf("service NetworkMode = %q, want none", in.HostConfig.NetworkMode)
	}
	capDropAll := false
	for _, c := range in.HostConfig.CapDrop {
		if strings.EqualFold(c, "ALL") {
			capDropAll = true
		}
	}
	if !capDropAll && (rt.Kind != gateiso.KindPodman || len(in.HostConfig.CapDrop) == 0) {
		t.Errorf("service CapDrop = %q, want ALL", in.HostConfig.CapDrop)
	}
	nnp := false
	for _, o := range in.HostConfig.SecurityOpt {
		if strings.HasPrefix(o, "no-new-privileges") {
			nnp = true
		}
	}
	if !nnp {
		t.Errorf("service SecurityOpt = %q, want no-new-privileges", in.HostConfig.SecurityOpt)
	}
	if in.HostConfig.Privileged {
		t.Errorf("service is privileged")
	}
	if len(in.HostConfig.PortBindings) != 0 || in.HostConfig.PublishAllPorts {
		t.Errorf("service publishes ports: %v (publish-all=%t)", in.HostConfig.PortBindings, in.HostConfig.PublishAllPorts)
	}
	if in.Config.User != "postgres" {
		t.Errorf("service user = %q, want postgres", in.Config.User)
	}
	if in.Config.Labels[gateiso.ServiceLabel] != string(gateiso.ServicePostgres) {
		t.Errorf("service labels = %v, want %s=postgres", in.Config.Labels, gateiso.ServiceLabel)
	}
	if _, ok := in.HostConfig.Tmpfs[gateiso.PostgresDataDir]; !ok {
		t.Errorf("service Tmpfs = %v, want %s (PGDATA must not land on a daemon-managed volume)", in.HostConfig.Tmpfs, gateiso.PostgresDataDir)
	}
	if len(in.Mounts) != 1 {
		t.Errorf("service mounts = %+v, want exactly the one named socket volume (no bind, no anonymous data volume)", in.Mounts)
	}
	for _, m := range in.Mounts {
		if m.Type != "volume" || m.Name != svc.Name || m.Destination != gateiso.PostgresSocketDir {
			t.Errorf("service mount %+v, want the named volume %s at %s", m, svc.Name, gateiso.PostgresSocketDir)
		}
	}
	vcmd := exec.CommandContext(ctx, rt.Kind.Binary(), append(endpoint, "volume", "inspect", "--format", "{{json .Labels}}", svc.Name)...)
	vcmd.Env = cliEnv
	if vout, err := vcmd.Output(); err != nil || !strings.Contains(string(vout), `"`+gateiso.ServiceLabel+`":"postgres"`) {
		t.Errorf("socket volume %s labels = %s (err %v), want %s=postgres (the manual-cleanup filter)", svc.Name, vout, err, gateiso.ServiceLabel)
	}
	for _, kv := range in.Config.Env {
		if v, ok := strings.CutPrefix(kv, "POSTGRES_PASSWORD="); ok {
			return v
		}
	}
	t.Errorf("service env carries no POSTGRES_PASSWORD: %q", in.Config.Env)
	return ""
}

// gateServiceProbeCmd is fixture (m)'s gate command. It always exits 0 and
// prints one key=value fact per line for the assertions.
func gateServiceProbeCmd(port int) string {
	pg := func(key, sql string) string {
		return `printf '` + key + `=%s\n' "$(psql -X -w -v ON_ERROR_STOP=1 "$FISHHAWK_TEST_PG_URL" -tAc "` + sql + `" 2>&1 | tr '\n' ' ')"`
	}
	superDSN := "postgres://postgres@/postgres?host=" + gateiso.MountPgSock + "&sslmode=disable"
	return strings.Join([]string{
		pg("select1", "select 1"),
		pg("role", "select rolsuper::text || ',' || rolcreatedb::text || ',' || rolcreaterole::text || ',' || rolbypassrls::text from pg_roles where rolname = current_user"),
		`printf 'createdb=%s\n' "$(psql -X -w -v ON_ERROR_STOP=1 "$FISHHAWK_TEST_PG_URL" -c 'CREATE DATABASE fh_e2e_probe' -c 'DROP DATABASE fh_e2e_probe' >/dev/null 2>&1 && echo ok || echo failed)"`,
		pg("createrole", "CREATE ROLE fh_e2e_probe"),
		pg("copyprogram", "COPY (SELECT 1) TO PROGRAM 'true'"),
		pg("grantexec", "GRANT pg_execute_server_program TO CURRENT_USER"),
		pg("createsuper", "CREATE ROLE fh_e2e_super SUPERUSER"),
		pg("altersuper", "ALTER ROLE postgres PASSWORD 'x'"),
		`printf 'superuser=%s\n' "$(psql -X -w '` + superDSN + `' -tAc 'select 1' 2>&1 | tr '\n' ' ')"`,
		`wget -q -T 3 -O /dev/null http://example.com/ && echo external=reached || echo external=failed`,
		fmt.Sprintf(`wget -q -T 3 -O /dev/null http://127.0.0.1:%d/ && echo loopback=reached || echo loopback=failed`, port),
		`printf 'ifaces=%s\n' "$(ls /sys/class/net | tr '\n' ' ')"`,
		`for s in /var/run/docker.sock /run/docker.sock /run/podman/podman.sock; do test -e $s && echo socket=$s; done`,
		`printf 'mountsocket=%s\n' "$(find /work /gocache /gomodcache /lintcache /run /var/run -type s 2>/dev/null | head -1)"`,
		`printf 'pgsocket=%s\n' "$(find /pgsock -type s 2>/dev/null | tr '\n' ' ')"`,
		`touch /pgsock/planted 2>/dev/null && echo pgsock=writable || echo pgsock=readonly`,
		`printf 'pgsockmount=%s\n' "$(awk '$2 == "/pgsock" {print $4}' /proc/mounts)"`,
		`printf 'passwdmount=%s\n' "$(awk '$2 == "/etc/passwd" {print $4}' /proc/mounts)"`,
		`id -un >/dev/null 2>&1 && printf 'user=%s\n' "$(id -un)" || echo user=unresolved`,
		`printf 'marker=%s\n' "$FISHHAWK_GATE_CONTAINER"`,
		`echo env-begin=1; env; echo env-end=1`,
		`echo done=ok`,
	}, "; ")
}

// envSection returns the `env` dump fixture (m) prints between its markers.
func envSection(out string) []string {
	_, rest, _ := strings.Cut(out, "env-begin=1\n")
	body, _, _ := strings.Cut(rest, "env-end=1")
	return strings.Split(strings.TrimSpace(body), "\n")
}

// (m) TestGateContainer_PostgresServiceReachableAndContained: with
// FISHHAWK_GATE_SERVICES=postgres, a daemon-dependent gate command reaches the
// runner-provisioned Postgres over the injected DSN (the read-only unix-socket
// mount) through the REAL container seam, as the gate role (#2137 approval
// condition 1, widened by #4050): CREATE DATABASE and CREATE ROLE work and the
// role is NOSUPERUSER with BYPASSRLS, while COPY … TO PROGRAM, granting
// pg_execute_server_program, creating a superuser and altering the superuser
// are refused, and the superuser cannot connect without the password that
// never enters the gate. The containment set still holds (no egress
// incl. a host loopback proven live first, no eth*, no daemon socket, /pgsock
// and /etc/passwd mounted read-only (asserted on the /proc/mounts options),
// the caller uid resolves through the runner's passwd mount,
// FISHHAWK_GATE_CONTAINER=1); the service itself is inspected on the host
// before the gate exec (inspectGateService); and after the exec the service
// container and its socket volume are gone. The counterfactual — the same
// psql DSN with services UNSET — must FAIL, proving the success is the
// provisioned socket and nothing else.
func TestGateContainer_PostgresServiceReachableAndContained(t *testing.T) {
	rt, _ := requireGateImage(t)
	image := requirePostgresImage(t, rt)
	port := listenLoopback(t)
	requireLoopbackAnswers(t, port)

	var logBuf strings.Builder
	liveServiceState(t, rt, image, "postgres", &logBuf)
	aux := recordAuxExec(t)
	calls := recordHostExec(t)
	var svc gateiso.PostgresService
	var superPassword string
	observed := 0
	prevObs := gateServiceObserver
	gateServiceObserver = func(ctx context.Context, r gateiso.Runtime, cliEnv []string, s gateiso.PostgresService) {
		observed++
		svc = s
		superPassword = inspectGateService(t, ctx, r, cliEnv, s)
	}
	t.Cleanup(func() { gateServiceObserver = prevObs })
	t.Cleanup(func() {
		if svc.Name != "" { // belt and braces should an assertion below fail first
			_, _ = runtimeHostCmd(t, rt, "rm", "-f", "-v", svc.Name)
			_, _ = runtimeHostCmd(t, rt, "volume", "rm", "-f", svc.Name)
		}
	})

	out, code := runBoundedGateCommand(context.Background(), gateServiceProbeCmd(port), t.TempDir(), filepath.Join(t.TempDir(), "lc"), 3*time.Minute)
	if observed != 1 || svc.Name == "" {
		t.Fatalf("gateServiceObserver called %d times (service %q), want once:\nexit %d\n%s\nlog:\n%s", observed, svc.Name, code, out, logBuf.String())
	}
	if code != 0 || outputField(out, "done") != "ok" {
		t.Fatalf("exit %d:\n%s\nlog:\n%s", code, out, logBuf.String())
	}

	// The daemon-dependent command, over the injected DSN, as the gate role.
	if got := outputField(out, "select1"); got != "1" {
		t.Errorf("psql over FISHHAWK_TEST_PG_URL printed %q, want 1:\n%s", got, out)
	}
	if got := outputField(out, "role"); got != "false,true,true,true" {
		t.Errorf("gate role (rolsuper,rolcreatedb,rolcreaterole,rolbypassrls) = %q, want false,true,true,true", got)
	}
	if got := outputField(out, "createdb"); got != "ok" {
		t.Errorf("CREATE DATABASE as the gate role = %q, want ok (pgtest needs CREATEDB)", got)
	}
	// CREATEROLE (#4050): the backend's RLS tests create NOBYPASSRLS probe roles.
	if got := outputField(out, "createrole"); !strings.Contains(got, "CREATE ROLE") || strings.Contains(got, "ERROR") {
		t.Errorf("CREATE ROLE as the gate role = %q, want the CREATE ROLE command tag (the backend suite needs CREATEROLE)", got)
	}
	for _, key := range []string{"copyprogram", "grantexec", "createsuper", "altersuper"} {
		if got := outputField(out, key); !strings.Contains(got, "permission denied") {
			t.Errorf("superuser-only operation %s from the gate DSN = %q, want a permission-denied refusal", key, got)
		}
	}
	if got := outputField(out, "superuser"); got == "1" || !strings.Contains(got, "password") {
		t.Errorf("superuser connect without its password = %q, want a password-required refusal", got)
	}

	// The superuser credential never reaches the gate container.
	if superPassword == "" {
		t.Fatal("observer did not capture the service superuser password")
	}
	if strings.Contains(out, superPassword) {
		t.Errorf("the service superuser password is visible inside the gate container:\n%s", out)
	}
	gateRun := containerRunArgv(t, *calls)
	for _, tok := range gateRun {
		if strings.Contains(tok, superPassword) {
			t.Errorf("the gate run argv carries the superuser password: %q", tok)
		}
	}
	env := envSection(out)
	var pgURL string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "FISHHAWK_TEST_PG_URL="); ok {
			pgURL = v
		}
		if strings.HasPrefix(kv, "POSTGRES_PASSWORD=") || strings.HasPrefix(kv, "PGPASSWORD=") {
			t.Errorf("superuser credential variable in the gate env: %s", kv)
		}
	}
	if pgURL != gateiso.PostgresGateURL {
		t.Errorf("gate FISHHAWK_TEST_PG_URL = %q, want %q", pgURL, gateiso.PostgresGateURL)
	}

	// Containment set.
	if outputField(out, "external") != "failed" {
		t.Errorf("external network reachable from the gate container:\n%s", out)
	}
	if outputField(out, "loopback") != "failed" {
		t.Errorf("host loopback listener on 127.0.0.1:%d reachable from the gate container:\n%s", port, out)
	}
	for _, i := range strings.Fields(outputField(out, "ifaces")) {
		if strings.HasPrefix(i, "eth") || strings.HasPrefix(i, "en") {
			t.Errorf("network interface %q present in the gate container", i)
		}
	}
	if got := outputField(out, "socket"); got != "" {
		t.Errorf("daemon socket %s present inside the gate container", got)
	}
	if got := outputField(out, "mountsocket"); got != "" {
		t.Errorf("unix socket %s visible outside /pgsock inside the gate container", got)
	}
	if got := strings.Fields(outputField(out, "pgsocket")); len(got) != 1 || got[0] != gateiso.MountPgSock+"/.s.PGSQL.5432" {
		t.Errorf("sockets under %s = %q, want exactly the Postgres socket", gateiso.MountPgSock, got)
	}
	// The touch is NOT the discriminating check for `:ro`: the daemon's
	// volume copy-up leaves the socket dir without other-write, so the gate
	// uid cannot create a file there even on a rw mount. The mount options
	// from /proc/mounts are what a dropped `:ro` turns red.
	if got := outputField(out, "pgsock"); got != "readonly" {
		t.Errorf("%s is %q from the gate container, want readonly", gateiso.MountPgSock, got)
	}
	for key, target := range map[string]string{"pgsockmount": gateiso.MountPgSock, "passwdmount": gateiso.MountPasswd} {
		if got := outputField(out, key); got != "ro" && !strings.HasPrefix(got, "ro,") {
			t.Errorf("%s mount options = %q in the gate container, want ro", target, got)
		}
	}
	if got := outputField(out, "user"); got == "" || got == "unresolved" {
		t.Errorf("caller uid %d does not resolve to a name in the gate container (passwd mount): %q", os.Getuid(), got)
	}
	if got := outputField(out, "marker"); got != "1" {
		t.Errorf("FISHHAWK_GATE_CONTAINER = %q in the gate container, want 1", got)
	}
	if !argvHasPair(gateRun, "-v", svc.Name+":"+gateiso.MountPgSock+":ro") {
		t.Errorf("gate run argv lacks the read-only socket mount -v %s:%s:ro: %q", svc.Name, gateiso.MountPgSock, gateRun)
	}

	// Teardown: rm -f -v then volume rm, and nothing left on the daemon.
	var sawRemove, sawVolumeRemove bool
	for _, c := range *aux {
		joined := strings.Join(c, " ")
		sawRemove = sawRemove || strings.HasSuffix(joined, " rm -f -v "+svc.Name)
		sawVolumeRemove = sawVolumeRemove || strings.HasSuffix(joined, " volume rm -f "+svc.Name)
	}
	if !sawRemove || !sawVolumeRemove {
		t.Errorf("teardown argv missing (rm -f -v: %t, volume rm: %t): %q", sawRemove, sawVolumeRemove, *aux)
	}
	if ps, err := runtimeHostCmd(t, rt, "ps", "-a", "--filter", "name="+svc.Name, "--format", "{{.Names}}"); err != nil || ps != "" {
		t.Errorf("service container %s still present after the exec (err %v): %q", svc.Name, err, ps)
	}
	if vols, err := runtimeHostCmd(t, rt, "volume", "ls", "-q", "--filter", "name="+svc.Name); err != nil || vols != "" {
		t.Errorf("socket volume %s still present after the exec (err %v): %q", svc.Name, err, vols)
	}
	log := logBuf.String()
	for _, want := range []string{`"event":"gate_service_provisioned"`, `"event":"gate_service_removed"`} {
		if !strings.Contains(log, want) || !strings.Contains(log, svc.Name) {
			t.Errorf("runner log lacks %s for %s:\n%s", want, svc.Name, log)
		}
	}
	for _, bad := range []string{`"event":"gate_service_cleanup_failed"`, `"event":"gate_passwd_unavailable"`} {
		if strings.Contains(log, bad) {
			t.Errorf("runner log carries %s:\n%s", bad, log)
		}
	}

	// Counterfactual: the same DSN with services UNSET has no socket to reach.
	liveServiceState(t, rt, image, "", io.Discard)
	before := len(*aux)
	cfCmd := `psql -X -w '` + gateiso.PostgresGateURL + `' -tAc 'select 1'`
	cfOut, cfCode := runBoundedGateCommand(context.Background(), cfCmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if cfCode == 0 || strings.TrimSpace(cfOut) == "1" {
		t.Errorf("counterfactual: psql with services unset exited %d printing %q, want a connection failure", cfCode, cfOut)
	}
	for _, c := range (*aux)[before:] {
		if strings.Contains(strings.Join(c, " "), gateiso.ServiceLabel) {
			t.Errorf("counterfactual: a gate service was provisioned with services unset: %q", c)
		}
	}
	dockerFixturesRan++
}

// (n) TestGateContainer_SelfHostPgtestSuite (OPT-IN on
// FISHHAWK_GATE_SELFHOST_IMAGE): the REAL pgtest suite of this repository's
// committed HEAD runs inside the built fishhawk-gate image through the
// container seam with the provisioned service — pgx and golang-migrate (the
// pgtest external branch: template bootstrap + MigrateUp + per-test databases)
// over the unix-socket DSN as the least-privilege role. Asserts exit 0, the
// PASS line, and no SKIP, so a silently skipped suite cannot pass. Counts
// toward the sentinel only when opted in and executed.
func TestGateContainer_SelfHostPgtestSuite(t *testing.T) {
	image := os.Getenv(gateSelfHostImageEnvVar)
	if image == "" {
		t.Skipf("%s unset: opt-in; build the gate image (`docker buildx build --load -t fishhawk-gate:local deploy/gate-image`) and set %s=fishhawk-gate:local",
			gateSelfHostImageEnvVar, gateSelfHostImageEnvVar)
	}
	rt, _ := requireGateImage(t)
	requirePostgresImage(t, rt)
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no source path")
	}
	top, err := exec.Command("git", "-C", filepath.Dir(self), "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("repository root not resolvable from %s: %v", self, err)
	}
	root := strings.TrimSpace(string(top))
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	checkout, err := materializeGateCheckout(context.Background(), root, strings.TrimSpace(string(head)), t.TempDir())
	if err != nil {
		t.Fatalf("materialize %s at HEAD: %v", root, err)
	}
	var logBuf strings.Builder
	liveServiceState(t, rt, image, "postgres", &logBuf)
	const testName = "TestSharedContainer_SharesAndIsolates"
	cmd := `cd backend && go test -count=1 -v -run '^` + testName + `$' ./internal/pgtest/`
	out, code := runBoundedGateCommand(context.Background(), cmd, checkout, filepath.Join(t.TempDir(), "lc"), 15*time.Minute)
	if code != 0 {
		t.Fatalf("in-container pgtest suite: exit %d:\n%s\nlog:\n%s", code, out, logBuf.String())
	}
	if !strings.Contains(out, "--- PASS: "+testName) {
		t.Errorf("no PASS line for %s:\n%s", testName, out)
	}
	if strings.Contains(out, "--- SKIP") {
		t.Errorf("the pgtest suite SKIPPED inside the gate container (a silent skip is not a pass):\n%s", out)
	}
	if !strings.Contains(logBuf.String(), `"event":"gate_service_removed"`) {
		t.Errorf("runner log lacks gate_service_removed:\n%s", logBuf.String())
	}
	// The positive evidence an operator walk records: the in-container
	// go test -v output with its PASS line.
	t.Logf("in-container pgtest output:\n%s", out)
	dockerFixturesRan++
}

// liveCacheState configures (does NOT install) a mode=container state on the
// REAL detected runtime with FISHHAWK_GATE_CACHE=cache, bound to (run, stage)
// — one runner process in fixture (o). Its cleanup is registered so a failing
// fixture still removes the volume.
func liveCacheState(t *testing.T, rt gateiso.Runtime, image, cache, run, stage string) *gateIsolationState {
	t.Helper()
	st, err := configureGateIsolation(fakeEnv(map[string]string{
		gateIsolationModeEnvVar: "container", gateImageEnvVar: image, gateCacheEnvVar: cache}), gateiso.DefaultProbes(), testLogWriter{t})
	if err != nil {
		t.Fatal(err)
	}
	st.detect = func(context.Context, gateiso.Probes) gateiso.Runtime { return rt }
	st.bindOwner(run, stage)
	t.Cleanup(st.cleanup)
	return st
}

// testLogWriter routes a state's runner log lines to t.Log, so a fixture
// failure shows why a cache step degraded.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// liveExecOn runs cmd through the real gate seam with st as the process-wide
// state, failing the fixture on a non-zero exit.
func liveExecOn(t *testing.T, st *gateIsolationState, cmd string) string {
	t.Helper()
	prev := gateIsolation
	gateIsolation = st
	defer func() { gateIsolation = prev }()
	out, code := runBoundedGateCommand(context.Background(), cmd, t.TempDir(), filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	return out
}

// lastContainerRunArgv returns the LAST gate `run` argv recorded by
// recordHostExec.
func lastContainerRunArgv(t *testing.T, calls [][]string) []string {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if c := calls[i]; len(c) > 4 && c[3] == "run" {
			return c
		}
	}
	t.Fatalf("no `run` argv recorded: %q", calls)
	return nil
}

// (o) TestGateContainer_CacheVolumeWarmWithinProcessFreshAcrossProcesses
// (E51.18 / #3967): state A (cache process) exec 1 runs as the runner uid,
// sees GOCACHE=/gatecache/gocache and can WRITE it (the prepare helper's
// ownership); A's exec 2 finds the marker (warm within a process); state B —
// a second runner process — mounts a different volume and finds nothing
// (fresh across processes); the positive control C (cache off) does NOT see
// its own exec-1 marker, so the warm assertion discriminates on the volume.
// The gate argv mounts the volume BY NAME (never an absolute host path)
// beside --network=none, and the volume carries the org.fishhawk.gate-cache
// label until A.cleanup() removes it.
func TestGateContainer_CacheVolumeWarmWithinProcessFreshAcrossProcesses(t *testing.T) {
	rt, image := requireGateImage(t)
	calls := recordHostExec(t)
	const (
		runA, stageA = "0000000a-0000-4000-8000-00000000000a", "0000000a-0000-4000-8000-0000000000a0"
		runB, stageB = "0000000b-0000-4000-8000-00000000000b", "0000000b-0000-4000-8000-0000000000b0"
	)
	write := `printf 'uid=%s\n' "$(id -u)"; printf 'gocache=%s\n' "$GOCACHE"; touch "$GOCACHE/marker" && echo marker=wrote`
	probe := `if test -f "$GOCACHE/marker"; then echo probe=warm; else echo probe=fresh; fi`

	a := liveCacheState(t, rt, image, "process", runA, stageA)
	out := liveExecOn(t, a, write)
	if got := outputField(out, "uid"); got != strconv.Itoa(os.Getuid()) {
		t.Errorf("A exec 1 uid = %q, want the runner's %d", got, os.Getuid())
	}
	if got := outputField(out, "gocache"); got != gateiso.GateGoCache {
		t.Errorf("A exec 1 GOCACHE = %q, want %q", got, gateiso.GateGoCache)
	}
	if outputField(out, "marker") != "wrote" {
		t.Fatalf("A exec 1 could not write its GOCACHE as the runner uid (prepare ownership):\n%s", out)
	}
	runA1 := lastContainerRunArgv(t, *calls)
	volA := argvMountSource(runA1, gateiso.MountGateCache)
	if !(gateiso.CacheVolume{Name: volA}).BelongsTo(runA, stageA) || filepath.IsAbs(volA) {
		t.Fatalf("A's /gatecache source %q is not its named cache volume: %q", volA, runA1)
	}
	if !slices.Contains(runA1, "--network=none") {
		t.Errorf("A's gate argv lacks --network=none: %q", runA1)
	}
	if got := outputField(liveExecOn(t, a, probe), "probe"); got != "warm" {
		t.Errorf("A exec 2 probe = %q, want warm (one volume per process, reused)", got)
	}

	b := liveCacheState(t, rt, image, "process", runB, stageB)
	if got := outputField(liveExecOn(t, b, probe), "probe"); got != "fresh" {
		t.Errorf("B probe = %q, want fresh (another process must never see A's cache)", got)
	}
	if volB := argvMountSource(lastContainerRunArgv(t, *calls), gateiso.MountGateCache); volB == "" || volB == volA {
		t.Errorf("B mounted %q, A mounted %q: want a distinct volume", volB, volA)
	}

	c := liveCacheState(t, rt, image, "off", runA, stageA)
	if out := liveExecOn(t, c, write); outputField(out, "marker") != "wrote" || outputField(out, "gocache") != gateiso.MountGoCache {
		t.Fatalf("C exec 1 (cache off) did not write its per-exec GOCACHE:\n%s", out)
	}
	if got := outputField(liveExecOn(t, c, probe), "probe"); got != "fresh" {
		t.Errorf("positive control C (cache off) probe = %q, want fresh: the warm assertion would not discriminate", got)
	}

	label, err := runtimeHostCmd(t, rt, "volume", "inspect", "--format", `{{ index .Labels "`+gateiso.CacheVolumeLabel+`" }}`, volA)
	if err != nil || label != string(gateiso.CacheModeProcess) {
		t.Errorf("volume inspect %s = %q / %v, want label %s=process before cleanup", volA, label, err, gateiso.CacheVolumeLabel)
	}
	a.cleanup()
	if out, err := runtimeHostCmd(t, rt, "volume", "inspect", volA); err == nil {
		t.Errorf("volume %s survives A.cleanup():\n%s", volA, out)
	}
	dockerFixturesRan++
}
