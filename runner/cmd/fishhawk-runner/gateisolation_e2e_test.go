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
// the pinned image. Every docker-gated fixture (a)–(g), (k) skips with the
// detected reason when no safe runtime or the image is unavailable, and
// increments dockerFixturesRan at its END so main_test.go's TestMain
// sentinel (approval condition 3) turns an all-skipped run on a docker host
// into a failure. The fallback fixtures (h)–(j) need no runtime.
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
var gateImageBinaries = []string{"git", "wget", "env", "sh", "cat", "ls", "sleep", "id", "stat", "test", "touch", "ln"}

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
// selection carries the real endpoint) and returns it.
func liveContainerState(t *testing.T, rt gateiso.Runtime, image string) *gateIsolationState {
	t.Helper()
	st, err := configureGateIsolation(fakeEnv(map[string]string{
		gateIsolationModeEnvVar: "container", gateImageEnvVar: image}), gateiso.DefaultProbes(), io.Discard)
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
	execBoundedHostArgvFn = func(ctx context.Context, argv []string, dir string, env []string, timeout time.Duration) (string, int) {
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
	execBoundedHostArgvFn = func(ctx context.Context, argv []string, dir string, env []string, timeout time.Duration) (string, int) {
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
