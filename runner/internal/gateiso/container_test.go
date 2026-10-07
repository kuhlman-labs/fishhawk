package gateiso

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// resolvedDir makes a real directory and returns its symlink-free path so
// the golden argv carries no macOS /var → /private/var indirection.
func resolvedDir(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type mountSet struct{ root, checkout, gocache, gomod, lint string }

func newMounts(t *testing.T) mountSet {
	t.Helper()
	root := shortTempDir(t)
	m := mountSet{
		checkout: resolvedDir(t, root, "co"),
		gocache:  resolvedDir(t, root, "gc"),
		gomod:    resolvedDir(t, root, "gm"),
		lint:     resolvedDir(t, root, "lc"),
	}
	m.root, _ = filepath.EvalSymlinks(root)
	if err := os.WriteFile(filepath.Join(m.checkout, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return m
}

// testSock is the validated daemon socket every rendered spec binds to; it
// need not exist for BuildArgv (ForbidSocketMounts keeps an unresolvable
// DaemonSocket literal).
const testSock = "/nonexistent/daemon.sock"

func specFor(m mountSet, rt Runtime) ContainerSpec {
	if rt.SocketPath == "" && rt.Kind != KindNone {
		rt.SocketPath = testSock
	}
	return ContainerSpec{
		Runtime: rt, Image: "docker.io/alpine/git:v2.47.2", Name: "fishhawk-gate-0a1b2c3d4e5f",
		Checkout: m.checkout, GoCache: m.gocache, GoModCache: m.gomod, LintCache: m.lint,
		UID: 501, GID: 20,
		Env:  []string{"GOFLAGS=-mod=mod", "GOPROXY=off"},
		Argv: []string{"/bin/sh", "-c", "scripts/test verify"},
	}
}

func TestBuildArgv_Golden(t *testing.T) {
	m := newMounts(t)
	policy := MountPolicy{Permitted: []string{m.root}}
	common := []string{
		"--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--workdir", "/work",
	}
	mounts := []string{
		"-v", m.checkout + ":/work", "-v", m.gocache + ":/gocache", "-v", m.gomod + ":/gomodcache", "-v", m.lint + ":/lintcache",
		"-e", "GOFLAGS=-mod=mod", "-e", "GOPROXY=off",
		"--entrypoint", "", "docker.io/alpine/git:v2.47.2", "/bin/sh", "-c", "scripts/test verify",
	}
	t.Run("docker", func(t *testing.T) {
		got, err := specFor(m, Runtime{Kind: KindDocker, Safe: true}).BuildArgv(policy)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{"docker", "--host", "unix://" + testSock, "run", "--rm", "--name", "fishhawk-gate-0a1b2c3d4e5f"}, common...)
		want = append(want, "--user", "501:20")
		want = append(want, mounts...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("argv mismatch\n got %q\nwant %q", got, want)
		}
	})
	t.Run("podman rootless", func(t *testing.T) {
		got, err := specFor(m, Runtime{Kind: KindPodman, Safe: true, Rootless: true}).BuildArgv(policy)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{"podman", "--url", "unix://" + testSock, "run", "--rm", "--name", "fishhawk-gate-0a1b2c3d4e5f"}, common...)
		want = append(want, "--userns=keep-id")
		want = append(want, mounts...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("argv mismatch\n got %q\nwant %q", got, want)
		}
	})
	t.Run("podman rootful uses --user", func(t *testing.T) {
		got, err := specFor(m, Runtime{Kind: KindPodman, Safe: true}).BuildArgv(policy)
		if err != nil {
			t.Fatal(err)
		}
		if idx(got, "--user") < 0 || idx(got, "--userns=keep-id") >= 0 {
			t.Fatalf("argv = %q", got)
		}
	})
}

func idx(argv []string, tok string) int {
	for i, a := range argv {
		if a == tok {
			return i
		}
	}
	return -1
}

// TestBuildArgv_EntrypointPrecedesImage pins approval condition (1): the
// --entrypoint element carries an EMPTY value and precedes the image, and
// the full gate argv follows the image explicitly.
func TestBuildArgv_EntrypointPrecedesImage(t *testing.T) {
	m := newMounts(t)
	spec := specFor(m, Runtime{Kind: KindDocker, Safe: true})
	got, err := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}})
	if err != nil {
		t.Fatal(err)
	}
	e, img := idx(got, "--entrypoint"), idx(got, spec.Image)
	if e < 0 || img < 0 || e >= img {
		t.Fatalf("--entrypoint at %d must precede image at %d: %q", e, img, got)
	}
	if got[e+1] != "" {
		t.Fatalf("--entrypoint value = %q, want empty", got[e+1])
	}
	if e+2 != img {
		t.Fatalf("--entrypoint '' must immediately precede the image: %q", got)
	}
	if !reflect.DeepEqual(got[img+1:], spec.Argv) {
		t.Fatalf("argv after image = %q, want %q", got[img+1:], spec.Argv)
	}
}

func TestBuildArgv_NetworkNoneFourMountsNoSocketToken(t *testing.T) {
	m := newMounts(t)
	got, err := specFor(m, Runtime{Kind: KindDocker, Safe: true}).BuildArgv(MountPolicy{Permitted: []string{m.root}})
	if err != nil {
		t.Fatal(err)
	}
	if idx(got, "--network=none") < 0 {
		t.Fatalf("no --network=none in %q", got)
	}
	n := 0
	for i, a := range got {
		if a == "-v" {
			n++
			if strings.Contains(got[i+1], "docker.sock") || strings.Contains(got[i+1], "podman.sock") || strings.Contains(got[i+1], "/var/run") {
				t.Fatalf("socket-bearing mount %q", got[i+1])
			}
		}
	}
	if n != 4 {
		t.Fatalf("expected exactly 4 -v tokens, got %d in %q", n, got)
	}
	for i, a := range got {
		if a == "-e" && strings.HasPrefix(got[i+1], "FISHHAWK_") {
			t.Fatalf("runner secret crossed into the container env: %q", got[i+1])
		}
	}
}

func TestBuildArgv_RefusedMountReturnsNoArgv(t *testing.T) {
	m := newMounts(t)
	listenUnix(t, m.checkout, "planted.sock")
	got, err := specFor(m, Runtime{Kind: KindDocker, Safe: true}).BuildArgv(MountPolicy{Permitted: []string{m.root}})
	if err == nil || !strings.Contains(err.Error(), "planted.sock") || !strings.Contains(err.Error(), "unix socket") {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Fatalf("refusal must return no argv, got %q", got)
	}
}

// TestBuildArgv_EndpointBindingPrecedesRun pins the endpoint binding: the
// runtime's global endpoint flag carries the VALIDATED socket and sits
// between the binary and `run` (a global flag after the subcommand is a
// `run` option the CLI rejects), KillArgv carries the same binding, and a
// Runtime with no validated SocketPath renders NOTHING — so a later docker
// context switch cannot redirect a bind-mount request. Deleting the
// EndpointArgs call in BuildArgv or KillArgv turns this red.
func TestBuildArgv_EndpointBindingPrecedesRun(t *testing.T) {
	m := newMounts(t)
	policy := MountPolicy{Permitted: []string{m.root}}
	for _, tc := range []struct {
		rt   Runtime
		flag string
	}{
		{Runtime{Kind: KindDocker, Safe: true, SocketPath: "/tmp/validated/docker.sock"}, "--host"},
		{Runtime{Kind: KindPodman, Safe: true, Rootless: true, SocketPath: "/tmp/validated/podman.sock"}, "--url"},
	} {
		spec := specFor(m, tc.rt)
		got, err := spec.BuildArgv(policy)
		if err != nil {
			t.Fatal(err)
		}
		want := "unix://" + tc.rt.SocketPath
		f, r := idx(got, tc.flag), idx(got, "run")
		if f != 1 || got[2] != want || r != 3 {
			t.Errorf("%s: argv must open <bin> %s %s run, got %q", tc.rt.Kind, tc.flag, want, got[:4])
		}
		if n := strings.Count(strings.Join(got, "\x00"), want); n != 1 {
			t.Errorf("%s: the socket must appear exactly once (the binding), %d times in %q", tc.rt.Kind, n, got)
		}
		kill := spec.KillArgv()
		if wantKill := []string{string(tc.rt.Kind), tc.flag, want, "rm", "-f", spec.Name}; !reflect.DeepEqual(kill, wantKill) {
			t.Errorf("%s: KillArgv = %q, want %q", tc.rt.Kind, kill, wantKill)
		}
	}
	unbound := specFor(m, Runtime{Kind: KindDocker, Safe: true})
	unbound.Runtime.SocketPath = ""
	if got, err := unbound.BuildArgv(policy); !errors.Is(err, ErrContainerSpec) || got != nil || !strings.Contains(err.Error(), "socket path") {
		t.Errorf("no SocketPath: got %q, %v; want ErrContainerSpec naming the socket path", got, err)
	}
	if got := unbound.KillArgv(); got != nil {
		t.Errorf("no SocketPath: KillArgv = %q, want nil", got)
	}
}

// TestBindEndpointEnv_DropsOverridesAndPins: every variable through which
// the CLI's endpoint or config dir could be redirected is dropped from the
// base env, the validated socket re-pinned and the CLI's config pinned to the
// runner's dir (E51.26 / #4046: an inherited DOCKER_CONFIG never survives,
// podman also gets REGISTRY_AUTH_FILE); the process environment is never
// consulted.
func TestBindEndpointEnv_DropsOverridesAndPins(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://process-env.invalid:2375")
	t.Setenv("DOCKER_CONFIG", "/process-env/.docker")
	const cfgDir = "/tmp/fishhawk-gate-docker-config-x"
	base := []string{"PATH=/usr/bin", "DOCKER_HOST=tcp://10.0.0.5:2376", "DOCKER_CONTEXT=remote",
		"CONTAINER_HOST=ssh://core@machine", "CONTAINER_CONNECTION=machine", "HOME=/home/r", "DOCKER_CONFIG=/home/r/.docker",
		"REGISTRY_AUTH_FILE=/home/r/auth.json"}
	for _, tc := range []struct {
		rt   Runtime
		pins []string
	}{
		{Runtime{Kind: KindDocker, SocketPath: "/tmp/v/docker.sock"},
			[]string{"DOCKER_HOST=unix:///tmp/v/docker.sock", "DOCKER_CONFIG=" + cfgDir}},
		{Runtime{Kind: KindPodman, SocketPath: "/tmp/v/podman.sock"},
			[]string{"CONTAINER_HOST=unix:///tmp/v/podman.sock", "DOCKER_CONFIG=" + cfgDir, "REGISTRY_AUTH_FILE=" + cfgDir + "/config.json"}},
	} {
		got, err := tc.rt.BindEndpointEnv(base, cfgDir)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{"PATH=/usr/bin", "HOME=/home/r"}, tc.pins...)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: env = %q, want %q", tc.rt.Kind, got, want)
		}
	}
	for _, rt := range []Runtime{{Kind: KindDocker}, {Kind: KindNone, SocketPath: "/tmp/v/s"}} {
		if got, err := rt.BindEndpointEnv(base, cfgDir); !errors.Is(err, ErrContainerSpec) || got != nil {
			t.Errorf("%+v: got %q, %v; want ErrContainerSpec", rt, got, err)
		}
	}
}

// TestBindEndpointEnv_RefusesMissingDockerConfig: with no (or a relative)
// docker config dir the binder refuses rather than fall back to an inherited
// DOCKER_CONFIG — which is exactly the interactive credential store the pin
// exists to keep out.
func TestBindEndpointEnv_RefusesMissingDockerConfig(t *testing.T) {
	base := []string{"PATH=/usr/bin", "DOCKER_CONFIG=/home/r/.docker"}
	for _, rt := range []Runtime{{Kind: KindDocker, SocketPath: "/tmp/v/docker.sock"}, {Kind: KindPodman, SocketPath: "/tmp/v/podman.sock"}} {
		for _, dir := range []string{"", "relative/cfg"} {
			got, err := rt.BindEndpointEnv(base, dir)
			if !errors.Is(err, ErrContainerSpec) || got != nil || !strings.Contains(err.Error(), "docker config dir") {
				t.Errorf("%s dir %q: got %q, %v; want ErrContainerSpec naming the docker config dir", rt.Kind, dir, got, err)
			}
		}
	}
}

func TestBuildArgv_IncompleteSpec(t *testing.T) {
	m := newMounts(t)
	base := specFor(m, Runtime{Kind: KindDocker, Safe: true})
	mutations := map[string]func(s *ContainerSpec){
		"no runtime":  func(s *ContainerSpec) { s.Runtime = Runtime{Kind: KindNone} },
		"no socket":   func(s *ContainerSpec) { s.Runtime.SocketPath = "" },
		"no image":    func(s *ContainerSpec) { s.Image = "" },
		"no name":     func(s *ContainerSpec) { s.Name = "" },
		"no argv":     func(s *ContainerSpec) { s.Argv = nil },
		"no checkout": func(s *ContainerSpec) { s.Checkout = "" },
		"no lint":     func(s *ContainerSpec) { s.LintCache = "" },
	}
	for name, mut := range mutations {
		s := base
		mut(&s)
		got, err := s.BuildArgv(MountPolicy{Permitted: []string{m.root}})
		if !errors.Is(err, ErrContainerSpec) || got != nil {
			t.Errorf("%s: got %q, %v; want ErrContainerSpec", name, got, err)
		}
	}
}

func TestKillArgv(t *testing.T) {
	s := ContainerSpec{Runtime: Runtime{Kind: KindPodman, SocketPath: "/run/user/501/podman/podman.sock"}, Name: "fishhawk-gate-abc"}
	if got := s.KillArgv(); !reflect.DeepEqual(got, []string{"podman", "--url", "unix:///run/user/501/podman/podman.sock", "rm", "-f", "fishhawk-gate-abc"}) {
		t.Fatalf("KillArgv = %q", got)
	}
}

func TestNewContainerName(t *testing.T) {
	re := regexp.MustCompile(`^fishhawk-gate-[0-9a-f]{12}$`)
	a, b := NewContainerName(), NewContainerName()
	if !re.MatchString(a) || !re.MatchString(b) || a == b {
		t.Fatalf("names %q %q", a, b)
	}
}

// --- ForbidSocketMounts ------------------------------------------------------

func mkdirs(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestForbidSocketMounts_DirectoryContainingSocketRefused(t *testing.T) {
	m := newMounts(t)
	deep := mkdirs(t, m.checkout, "a", "b")
	listenUnix(t, deep, "s.sock") // depth 3
	err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, m.checkout)
	if err == nil || !strings.Contains(err.Error(), "s.sock") || !strings.Contains(err.Error(), "depth 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestForbidSocketMounts_SymlinkToSocketRefused(t *testing.T) {
	m := newMounts(t)
	outside := shortTempDir(t)
	sock := listenUnix(t, outside, "o.sock")
	if err := os.Symlink(sock, filepath.Join(m.checkout, "vendor")); err != nil {
		t.Fatal(err)
	}
	err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, m.checkout)
	if err == nil || !strings.Contains(err.Error(), "symlink to unix socket") {
		t.Fatalf("err = %v", err)
	}
}

func TestForbidSocketMounts_InnocuousSymlinkToVarRunRefused(t *testing.T) {
	if _, err := os.Stat("/var/run"); err != nil {
		t.Skip("host has no /var/run")
	}
	m := newMounts(t)
	link := filepath.Join(m.root, "assets")
	if err := os.Symlink("/var/run", link); err != nil {
		t.Fatal(err)
	}
	// Permitted deliberately covers the resolved target's ancestor "/" so
	// that ONLY the forbidden-root rule can refuse it.
	err := ForbidSocketMounts(MountPolicy{Permitted: []string{"/"}}, link)
	if err == nil || !strings.Contains(err.Error(), "under forbidden root") {
		t.Fatalf("err = %v", err)
	}
	// On Linux /var/run is itself a symlink to /run, so the refusal names
	// the RESOLVED root ("/run"); on macOS it is a real directory and the
	// message names "/var/run". Accept either the link path or the
	// resolved root — the platform decides which one the control reports.
	want := "/var/run"
	if resolved, rerr := filepath.EvalSymlinks("/var/run"); rerr == nil {
		want = resolved
	}
	if !strings.Contains(err.Error(), `"`+want+`"`) && !strings.Contains(err.Error(), "/var/run") {
		t.Fatalf("err = %v: names neither %q nor /var/run", err, want)
	}
}

func TestForbidSocketMounts_ResolvedUnderRunRefused(t *testing.T) {
	n := 0
	for _, root := range []string{"/var/run", "/run"} {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		n++
		err := ForbidSocketMounts(MountPolicy{Permitted: []string{"/"}}, root)
		if err == nil || !strings.Contains(err.Error(), "forbidden root") {
			t.Fatalf("%s: err = %v", root, err)
		}
	}
	if n == 0 {
		t.Skip("host has neither /run nor /var/run")
	}
	if underRoot("/runway", "/run") || underRoot("/var/runner", "/var/run") || !underRoot("/run/x", "/run") || !underRoot("/run", "/run") {
		t.Fatal("underRoot must compare by path component")
	}
}

func TestForbidSocketMounts_PlainSocketPathRefused(t *testing.T) {
	m := newMounts(t)
	sock := listenUnix(t, m.checkout, "d.sock")
	err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, sock)
	if err == nil || !strings.Contains(err.Error(), "is a unix socket") {
		t.Fatalf("err = %v", err)
	}
}

func TestForbidSocketMounts_SymlinkOutsidePermittedRefused(t *testing.T) {
	m := newMounts(t)
	outside, _ := filepath.EvalSymlinks(shortTempDir(t))
	link := filepath.Join(m.root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, link)
	if err == nil || !strings.Contains(err.Error(), "outside the permitted roots") {
		t.Fatalf("err = %v", err)
	}
}

func TestForbidSocketMounts_SymlinkInsidePermittedAccepted(t *testing.T) {
	m := newMounts(t)
	link := filepath.Join(m.root, "link")
	if err := os.Symlink(m.checkout, link); err != nil {
		t.Fatal(err)
	}
	if err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, link); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	// A permitted root given through its own symlinked spelling still matches.
	if err := ForbidSocketMounts(MountPolicy{Permitted: []string{link}}, link); err != nil {
		t.Fatalf("unexpected refusal via symlinked permitted root: %v", err)
	}
}

func TestForbidSocketMounts_DaemonSocketInsideSourceRefused(t *testing.T) {
	m := newMounts(t)
	// Twelve levels deep: beyond the walk bound, so ONLY the explicit
	// DaemonSocket rule can catch it.
	deep := mkdirs(t, m.checkout, "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b")
	sock := listenUnix(t, deep, "d.sock")
	if err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, m.checkout); err != nil {
		t.Fatalf("walk must not reach depth 12: %v", err)
	}
	err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}, DaemonSocket: sock}, m.checkout)
	if err == nil || !strings.Contains(err.Error(), "daemon socket") {
		t.Fatalf("err = %v", err)
	}
}

// TestForbidSocketMounts_DepthBound pins the documented LIMIT (approval
// condition 6): a socket at depth 8 is refused, at depth 9 it is not seen.
func TestForbidSocketMounts_DepthBound(t *testing.T) {
	m := newMounts(t)
	d7 := mkdirs(t, m.checkout, "1", "2", "3", "4", "5", "6", "7")
	l := listenUnix(t, d7, "s8.sock") // depth 8
	err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, m.checkout)
	if err == nil || !strings.Contains(err.Error(), "depth 8") {
		t.Fatalf("depth 8 must be refused: %v", err)
	}
	os.Remove(l)
	d8 := mkdirs(t, d7, "8")
	listenUnix(t, d8, "s9.sock") // depth 9: beyond the bound, documented residual
	if err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, m.checkout); err != nil {
		t.Fatalf("depth 9 lies beyond the documented bound and must not be refused: %v", err)
	}
	if err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}, MaxDepth: 9}, m.checkout); err == nil || !strings.Contains(err.Error(), "depth 9") {
		t.Fatalf("MaxDepth=9 must reach depth 9: %v", err)
	}
	if DefaultMaxMountDepth != 8 {
		t.Fatalf("DefaultMaxMountDepth = %d; the documented bound is 8 (update README + this test together)", DefaultMaxMountDepth)
	}
}

func TestForbidSocketMounts_PermittedDirAccepted(t *testing.T) {
	m := newMounts(t)
	mkdirs(t, m.checkout, "pkg", "sub")
	if err := os.WriteFile(filepath.Join(m.checkout, "pkg", "sub", "x.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ForbidSocketMounts(MountPolicy{}, m.checkout, m.gocache, m.gomod, m.lint); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestForbidSocketMounts_RelativeEmptyUnresolvableRefused(t *testing.T) {
	m := newMounts(t)
	for _, src := range []string{"", "relative/dir"} {
		if err := ForbidSocketMounts(MountPolicy{}, src); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("%q: err = %v", src, err)
		}
	}
	dangling := filepath.Join(m.root, "dangling")
	if err := os.Symlink(filepath.Join(m.root, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := ForbidSocketMounts(MountPolicy{Permitted: []string{m.root}}, dangling); err == nil || !strings.Contains(err.Error(), "unresolvable") {
		t.Errorf("dangling: err = %v", err)
	}
	if err := ForbidSocketMounts(MountPolicy{}, filepath.Join(m.root, "missing")); err == nil {
		t.Error("missing source accepted")
	}
}

// --- ContainerEnv -----------------------------------------------------------

func TestContainerEnv_AllowListPinsExtras(t *testing.T) {
	sanitized := []string{
		"PATH=/usr/bin", "HOME=/Users/op", "FISHHAWK_API_TOKEN=secret", "ANTHROPIC_API_KEY=k",
		"TZ=UTC", "LANG=C.UTF-8", "TERM=xterm", "LC_ALL=C", "CGO_ENABLED=0", "GOFLAGS=-mod=mod",
		"GOCACHE=/Users/op/Library/Caches/go-build", "GOPROXY=https://proxy.golang.org", "GOTOOLCHAIN=auto",
		"GOLANGCI_LINT_CACHE=/host/lint", "NOEQUALS",
	}
	got := ContainerEnv(sanitized, []string{"FISHHAWK_VERIFY_LOCK_OWNER=runner", "GOPROXY=direct"})
	want := []string{
		"TZ=UTC", "LANG=C.UTF-8", "TERM=xterm", "LC_ALL=C", "CGO_ENABLED=0", "GOFLAGS=-mod=mod",
		"HOME=/tmp", "GOPATH=/tmp/gopath", "GOCACHE=/gocache", "GOMODCACHE=/gomodcache", "GOLANGCI_LINT_CACHE=/lintcache",
		"GOTOOLCHAIN=local", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "FISHHAWK_GATE_CONTAINER=1",
		"FISHHAWK_VERIFY_LOCK_OWNER=runner", "GOPROXY=direct",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ContainerEnv\n got %q\nwant %q", got, want)
	}
	seen := map[string]int{}
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		seen[k]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("key %s appears %d times", k, n)
		}
	}
}

func TestContainerEnv_DefaultsWithoutExtras(t *testing.T) {
	got := ContainerEnv(nil, []string{"NOEQUALS"})
	if !reflect.DeepEqual(got, containerEnvPins) {
		t.Fatalf("got %q", got)
	}
	if idx(got, "GOPROXY=off") < 0 {
		t.Fatalf("GOPROXY=off pin missing: %q", got)
	}
}

// TestContainerEnv_HostGoVarsNeverCross pins #4048: a host-specific Go variable
// in the sanitized env (a toolchain-switching macOS host exports a darwin
// GOROOT to the runner process) must never reach the Linux gate container, in
// ContainerEnv's output or as a BuildArgv `-e` token. The platform-independent
// tuning knobs survive verbatim and the runner pins still win.
func TestContainerEnv_HostGoVarsNeverCross(t *testing.T) {
	m := newMounts(t)
	darwinRoot := "/Users/x/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.6.darwin-arm64"
	dropped := []string{
		"GOROOT=" + darwinRoot, "GOOS=darwin", "GOARCH=arm64", "GOBIN=/Users/x/go/bin",
		"GOWORK=/Users/x/go.work", "GOENV=/Users/x/.config/go/env", "GOTOOLDIR=" + darwinRoot + "/pkg/tool/darwin_arm64",
		"GOHOSTOS=darwin", "GOHOSTARCH=arm64", "GOMOD=/Users/x/go.mod", "GOEXE=", "GOGCCFLAGS=-fPIC -arch arm64",
		"GOVERSION=go1.25.6", "GOCACHEPROG=/Users/x/cacheprog", "GOCOVERDIR=/Users/x/cov", "GOTMPDIR=/Users/x/tmp",
		"GOAUTH=netrc", "GOOGLE_API_KEY=secret",
	}
	for _, kv := range dropped {
		key, _, _ := strings.Cut(kv, "=")
		t.Run("drops_"+key, func(t *testing.T) {
			env := ContainerEnv([]string{kv, "TZ=UTC"}, nil)
			for _, e := range env {
				if k, _, _ := strings.Cut(e, "="); k == key {
					t.Fatalf("%s crossed into the container env: %q", key, env)
				}
			}
			if idx(env, "TZ=UTC") < 0 {
				t.Fatalf("control: TZ=UTC must still cross: %q", env)
			}
			spec := specFor(m, Runtime{Kind: KindDocker, Safe: true})
			spec.Env = env
			argv, err := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}})
			if err != nil {
				t.Fatal(err)
			}
			for i, a := range argv {
				if a == "-e" && strings.HasPrefix(argv[i+1], key+"=") {
					t.Fatalf("BuildArgv carries -e %s: %q", argv[i+1], argv)
				}
			}
		})
	}

	t.Run("tuning_knobs_survive_verbatim", func(t *testing.T) {
		keep := []string{
			"GODEBUG=panicnil=1", "GOEXPERIMENT=jsonv2", "GOMAXPROCS=2", "GOMEMLIMIT=2GiB", "GOGC=50",
			"GOTRACEBACK=all", "GOFIPS140=on", "GO111MODULE=on",
		}
		env := ContainerEnv(keep, nil)
		for _, kv := range keep {
			if idx(env, kv) < 0 {
				t.Errorf("%s did not survive: %q", kv, env)
			}
		}
	})

	t.Run("inherited_pinned_keys_come_out_as_the_pins", func(t *testing.T) {
		env := ContainerEnv([]string{
			"GOTOOLCHAIN=auto", "GOPROXY=https://proxy.golang.org", "GOCACHE=/Users/x/Library/Caches/go-build",
			"GOPATH=/Users/x/go", "GOMODCACHE=/Users/x/go/pkg/mod",
		}, nil)
		for _, want := range []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOCACHE=" + MountGoCache, "GOPATH=/tmp/gopath", "GOMODCACHE=" + MountGoModCache} {
			if idx(env, want) < 0 {
				t.Errorf("pin %s missing: %q", want, env)
			}
			key, _, _ := strings.Cut(want, "=")
			n := 0
			for _, e := range env {
				if k, _, _ := strings.Cut(e, "="); k == key {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s appears %d times: %q", key, n, env)
			}
		}
	})
}

// TestContainerGoflags pins the GOFLAGS rules: an absolute-path -flag=value
// field names a host file and is dropped; relative values and bare flags are
// kept byte-identical; a quote character drops the whole variable (Go parses
// GOFLAGS quote-aware, so a quoted field can hide a host path from a
// whitespace split); a value with nothing left omits the key.
func TestContainerGoflags(t *testing.T) {
	rows := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"plain kept", "-mod=mod", "-mod=mod", true},
		{"absolute modfile dropped", "-mod=mod -modfile=/Users/x/alt.mod", "-mod=mod", true},
		{"overlay alone omits the key", "-overlay=/Users/x/o.json", "", false},
		{"relative modfile kept", "-modfile=alt.mod", "-modfile=alt.mod", true},
		{"pgo dropped, bare boolean kept", "-pgo=/x/default.pgo -trimpath", "-trimpath", true},
		{"double-dash toolexec dropped", "--toolexec=/Users/x/t -mod=mod", "-mod=mod", true},
		{"pkgdir dropped", "-pkgdir=/Users/x/pkg -v", "-v", true},
		{"quoted value fails closed", "-mod=mod '-modfile=/Users/x/a b.mod'", "", false},
		{"double-quoted value fails closed", `-mod=mod "-modfile=/Users/x/a b.mod"`, "", false},
		{"empty omits the key", "", "", false},
		{"whitespace-only omits the key", "  \t ", "", false},
		{"extra spaces re-joined with one", "-mod=mod   -v", "-mod=mod -v", true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, ok := containerGoflags(r.in)
			if got != r.want || ok != r.wantOK {
				t.Fatalf("containerGoflags(%q) = (%q, %v), want (%q, %v)", r.in, got, ok, r.want, r.wantOK)
			}
		})
	}
}

// TestContainerEnv_GoflagsSanitizedInProjection pins the call site: ContainerEnv
// applies containerGoflags to the sanitized GOFLAGS only, omits the key when
// nothing survives, and leaves a runner-supplied extra untouched.
func TestContainerEnv_GoflagsSanitizedInProjection(t *testing.T) {
	has := func(env []string, prefix string) bool {
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	env := ContainerEnv([]string{"GOFLAGS=-mod=mod -modfile=/Users/x/alt.mod"}, nil)
	if idx(env, "GOFLAGS=-mod=mod") < 0 {
		t.Errorf("absolute-path field not stripped: %q", env)
	}
	env = ContainerEnv([]string{"GOFLAGS=-overlay=/Users/x/o.json"}, nil)
	if has(env, "GOFLAGS=") {
		t.Errorf("all-dropped GOFLAGS must omit the key, not emit it empty: %q", env)
	}
	env = ContainerEnv([]string{"GOFLAGS=-mod=mod '-modfile=/Users/x/a b.mod'"}, nil)
	if has(env, "GOFLAGS=") {
		t.Errorf("quoted GOFLAGS must be dropped whole: %q", env)
	}
	env = ContainerEnv([]string{"GOFLAGS=-mod=mod"}, []string{"GOFLAGS=-modfile=/tmp/extra.mod"})
	if idx(env, "GOFLAGS=-modfile=/tmp/extra.mod") < 0 {
		t.Errorf("runner-supplied extras must not be filtered: %q", env)
	}
}

// TestContainerEnv_PinsGateContainerMarker: every container exec carries
// FISHHAWK_GATE_CONTAINER=1, and a sanitized-env value cannot unset it (the
// allow-list drops it; the pin re-appends).
func TestContainerEnv_PinsGateContainerMarker(t *testing.T) {
	got := ContainerEnv([]string{"FISHHAWK_GATE_CONTAINER=0", "GOFLAGS=x"}, nil)
	if idx(got, "FISHHAWK_GATE_CONTAINER=1") < 0 || idx(got, "FISHHAWK_GATE_CONTAINER=0") >= 0 {
		t.Fatalf("marker not pinned: %q", got)
	}
}

// TestContainerEnv_ServiceEnvWinsOverExtras: WithServiceEnv applies the
// provisioned DSN LAST, so an extras (or any earlier) value cannot redirect it.
func TestContainerEnv_ServiceEnvWinsOverExtras(t *testing.T) {
	svc := fixedService()
	base := ContainerEnv(nil, []string{"FISHHAWK_TEST_PG_URL=postgres://attacker@evil/x", "GOFLAGS=y"})
	got := WithServiceEnv(base, append(svc.GateEnv(), "NOEQUALS"))
	n := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "FISHHAWK_TEST_PG_URL=") {
			n++
		}
	}
	if n != 1 || got[len(got)-1] != "FISHHAWK_TEST_PG_URL="+PostgresGateURL {
		t.Fatalf("service DSN not applied last and alone: %q", got)
	}
	if idx(got, "NOEQUALS") >= 0 || idx(got, "GOFLAGS=y") < 0 {
		t.Errorf("WithServiceEnv mangled the base env: %q", got)
	}
	if idx(base, "FISHHAWK_TEST_PG_URL="+PostgresGateURL) >= 0 {
		t.Error("WithServiceEnv mutated its input")
	}
}

// --- passwd + service mounts -------------------------------------------------

func TestBuildArgv_ServiceMountAndPasswdReadOnly(t *testing.T) {
	m := newMounts(t)
	pw := filepath.Join(m.root, "passwd-1")
	if err := os.WriteFile(pw, []byte("root:x:0:0::/:/bin/sh\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	spec := specFor(m, Runtime{Kind: KindDocker, Safe: true})
	spec.PasswdFile = pw
	spec.ServiceMounts = []ServiceMount{fixedService().GateMount()}
	got, err := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	want := "-v " + m.lint + ":/lintcache -v " + pw + ":/etc/passwd:ro -v fishhawk-gate-svc-0a1b2c3d4e5f:/pgsock:ro -e "
	if !strings.Contains(joined, want) {
		t.Fatalf("argv %q lacks %q", joined, want)
	}
	// Without the optional pieces the shape is the four-mount golden.
	spec.PasswdFile, spec.ServiceMounts = "", nil
	plain, _ := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}})
	if strings.Contains(strings.Join(plain, " "), ":ro") {
		t.Errorf("read-only mounts emitted with none configured: %q", plain)
	}
}

// TestBuildArgv_ServiceMountRefusesNonServiceVolume: a host path, the daemon
// socket, a traversal, a near-miss name, a bad target or a writable mount is
// refused with NO argv — without the check BuildArgv would emit e.g.
// `-v /var/run:/pgsock:ro`.
func TestBuildArgv_ServiceMountRefusesNonServiceVolume(t *testing.T) {
	m := newMounts(t)
	ok := fixedService().GateMount()
	for _, sm := range []ServiceMount{
		{Volume: "/var/run", Target: "/pgsock", ReadOnly: true},
		{Volume: "/Users/x", Target: "/pgsock", ReadOnly: true},
		{Volume: "docker.sock", Target: "/pgsock", ReadOnly: true},
		{Volume: "../x", Target: "/pgsock", ReadOnly: true},
		{Volume: "fishhawk-gate-svc-0A1B2C3D4E5F", Target: "/pgsock", ReadOnly: true},
		{Volume: "fishhawk-gate-svc-0a1b2c3d4e5f:/x", Target: "/pgsock", ReadOnly: true},
		{Volume: ok.Volume, Target: "pgsock", ReadOnly: true},
		{Volume: ok.Volume, Target: "/pg/../sock", ReadOnly: true},
		{Volume: ok.Volume, Target: "/", ReadOnly: true},
		{Volume: ok.Volume, Target: "/pgsock", ReadOnly: false},
	} {
		spec := specFor(m, Runtime{Kind: KindDocker, Safe: true})
		spec.ServiceMounts = []ServiceMount{ok, sm}
		argv, err := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}})
		if err == nil || argv != nil {
			t.Errorf("%+v: argv %q, err %v; want refusal and nil argv", sm, argv, err)
		}
		if strings.Contains(strings.Join(argv, " "), sm.Volume+":"+sm.Target) {
			t.Errorf("%+v rendered", sm)
		}
	}
}

// TestBuildArgv_PasswdFileThroughMountGuard: the passwd file is a bind-mount
// source like the checkout, so a symlink resolving outside the permitted
// roots (or onto a socket) is refused.
func TestBuildArgv_PasswdFileThroughMountGuard(t *testing.T) {
	m := newMounts(t)
	outside := shortTempDir(t)
	target := filepath.Join(outside, "passwd")
	if err := os.WriteFile(target, []byte("x\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m.root, "passwd-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	spec := specFor(m, Runtime{Kind: KindDocker, Safe: true})
	spec.PasswdFile = link
	argv, err := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}})
	if err == nil || argv != nil || !strings.Contains(err.Error(), "outside the permitted roots") {
		t.Fatalf("argv %q, err %v; want the symlinked passwd file refused", argv, err)
	}
	spec.PasswdFile = listenUnix(t, m.root, "p.sock")
	if argv, err := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}}); err == nil || argv != nil {
		t.Fatalf("socket passwd file accepted: %q", argv)
	}
}

// --- cache volume (#3967) ----------------------------------------------------

// volumeSpecFor is specFor in the volume form: the GoCache/LintCache binds
// are left empty and the spec mounts the fixed cache volume.
func volumeSpecFor(m mountSet, rt Runtime) ContainerSpec {
	s := specFor(m, rt)
	s.GoCache, s.LintCache = "", ""
	s.CacheVolume = testCacheVolume
	return s
}

// TestBuildArgv_CacheVolumeGolden pins the volume form: the gocache and
// lintcache binds are gone and the cache volume is mounted read-write at
// /gatecache after the module cache.
func TestBuildArgv_CacheVolumeGolden(t *testing.T) {
	m := newMounts(t)
	policy := MountPolicy{Permitted: []string{m.root}}
	common := []string{
		"--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--workdir", "/work",
	}
	mounts := []string{
		"-v", m.checkout + ":/work", "-v", m.gomod + ":/gomodcache", "-v", testCacheVolume + ":/gatecache",
		"-e", "GOFLAGS=-mod=mod", "-e", "GOPROXY=off",
		"--entrypoint", "", "docker.io/alpine/git:v2.47.2", "/bin/sh", "-c", "scripts/test verify",
	}
	for _, tc := range []struct {
		rt   Runtime
		head []string
		user []string
	}{
		{Runtime{Kind: KindDocker, Safe: true}, []string{"docker", "--host", "unix://" + testSock}, []string{"--user", "501:20"}},
		{Runtime{Kind: KindPodman, Safe: true, Rootless: true}, []string{"podman", "--url", "unix://" + testSock}, []string{"--userns=keep-id"}},
	} {
		got, err := volumeSpecFor(m, tc.rt).BuildArgv(policy)
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]string{}, tc.head...), "run", "--rm", "--name", "fishhawk-gate-0a1b2c3d4e5f")
		want = append(want, common...)
		want = append(want, tc.user...)
		want = append(want, mounts...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s argv mismatch\n got %q\nwant %q", tc.rt.Kind, got, want)
		}
	}
	// Bind sources set alongside a cache volume are not rendered.
	both := specFor(m, Runtime{Kind: KindDocker, Safe: true})
	both.CacheVolume = testCacheVolume
	got, err := both.BuildArgv(policy)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, ":/gocache") || strings.Contains(joined, ":/lintcache") || !strings.Contains(joined, testCacheVolume+":/gatecache ") {
		t.Fatalf("volume form rendered a cache bind: %q", got)
	}
	if strings.Contains(joined, testCacheVolume+":/gatecache:ro") {
		t.Fatalf("cache volume mounted read-only: %q", got)
	}
}

// TestBuildArgv_CacheVolumeRefusesNonCacheVolume: a host path, the daemon
// socket, a service volume, an unscoped, upper-case or mount-suffixed name is
// refused with NO argv — without the check BuildArgv would emit e.g.
// `-v /var/run/docker.sock:/gatecache`. An empty CacheVolume is not a
// refusal: it selects the bind form.
func TestBuildArgv_CacheVolumeRefusesNonCacheVolume(t *testing.T) {
	m := newMounts(t)
	policy := MountPolicy{Permitted: []string{m.root}}
	for _, vol := range []string{
		"/var/run/docker.sock",
		m.gocache,
		"docker.sock",
		"fishhawk-gate-svc-0a1b2c3d4e5f",
		"fishhawk-gate-cache-0a1b2c3d4e5f",
		strings.ToUpper(testCacheVolume),
		testCacheVolume + ":/etc",
		"../" + testCacheVolume,
	} {
		spec := volumeSpecFor(m, Runtime{Kind: KindDocker, Safe: true})
		spec.CacheVolume = vol
		argv, err := spec.BuildArgv(policy)
		if !errors.Is(err, ErrContainerSpec) || argv != nil || !strings.Contains(err.Error(), "not a gate cache volume") {
			t.Errorf("%q: argv %q, err %v; want the cache volume refused with nil argv", vol, argv, err)
		}
	}
	spec := specFor(m, Runtime{Kind: KindDocker, Safe: true})
	spec.CacheVolume = ""
	argv, err := spec.BuildArgv(policy)
	if err != nil || !strings.Contains(strings.Join(argv, " "), m.gocache+":/gocache") || strings.Contains(strings.Join(argv, " "), "/gatecache") {
		t.Fatalf("empty CacheVolume must select the bind form: %q, %v", argv, err)
	}
}

// TestBuildArgv_CacheVolumeStillGuardsCheckout: the volume form keeps
// ForbidSocketMounts over the checkout, the module cache and the passwd file.
func TestBuildArgv_CacheVolumeStillGuardsCheckout(t *testing.T) {
	for _, plant := range []string{"checkout", "gomod"} {
		m := newMounts(t)
		dir := m.checkout
		if plant == "gomod" {
			dir = m.gomod
		}
		listenUnix(t, dir, "planted.sock")
		got, err := volumeSpecFor(m, Runtime{Kind: KindDocker, Safe: true}).BuildArgv(MountPolicy{Permitted: []string{m.root}})
		if err == nil || got != nil || !strings.Contains(err.Error(), "planted.sock") {
			t.Errorf("%s: socket accepted in volume form: %q, %v", plant, got, err)
		}
	}
	m := newMounts(t)
	spec := volumeSpecFor(m, Runtime{Kind: KindDocker, Safe: true})
	spec.PasswdFile = listenUnix(t, m.root, "p.sock")
	if got, err := spec.BuildArgv(MountPolicy{Permitted: []string{m.root}}); err == nil || got != nil {
		t.Errorf("socket passwd file accepted in volume form: %q", got)
	}
}

// TestBuildArgv_CacheVolumeIncompleteSpec: the volume form still requires the
// checkout and the module cache, and no longer requires the cache binds.
func TestBuildArgv_CacheVolumeIncompleteSpec(t *testing.T) {
	m := newMounts(t)
	policy := MountPolicy{Permitted: []string{m.root}}
	for name, mut := range map[string]func(s *ContainerSpec){
		"no checkout": func(s *ContainerSpec) { s.Checkout = "" },
		"no gomod":    func(s *ContainerSpec) { s.GoModCache = "" },
	} {
		s := volumeSpecFor(m, Runtime{Kind: KindDocker, Safe: true})
		mut(&s)
		if got, err := s.BuildArgv(policy); !errors.Is(err, ErrContainerSpec) || got != nil {
			t.Errorf("%s: got %q, %v; want ErrContainerSpec", name, got, err)
		}
	}
	if got, err := volumeSpecFor(m, Runtime{Kind: KindDocker, Safe: true}).BuildArgv(policy); err != nil || got == nil {
		t.Errorf("volume form without cache binds refused: %q, %v", got, err)
	}
}
