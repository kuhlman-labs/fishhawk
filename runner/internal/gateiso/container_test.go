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
// the CLI's endpoint could be redirected is dropped from the base env and the
// validated socket re-pinned; the process environment is never consulted.
func TestBindEndpointEnv_DropsOverridesAndPins(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://process-env.invalid:2375")
	base := []string{"PATH=/usr/bin", "DOCKER_HOST=tcp://10.0.0.5:2376", "DOCKER_CONTEXT=remote",
		"CONTAINER_HOST=ssh://core@machine", "CONTAINER_CONNECTION=machine", "HOME=/home/r", "DOCKER_CONFIG=/home/r/.docker"}
	for _, tc := range []struct {
		rt  Runtime
		pin string
	}{
		{Runtime{Kind: KindDocker, SocketPath: "/tmp/v/docker.sock"}, "DOCKER_HOST=unix:///tmp/v/docker.sock"},
		{Runtime{Kind: KindPodman, SocketPath: "/tmp/v/podman.sock"}, "CONTAINER_HOST=unix:///tmp/v/podman.sock"},
	} {
		got, err := tc.rt.BindEndpointEnv(base)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"PATH=/usr/bin", "HOME=/home/r", "DOCKER_CONFIG=/home/r/.docker", tc.pin}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: env = %q, want %q", tc.rt.Kind, got, want)
		}
	}
	for _, rt := range []Runtime{{Kind: KindDocker}, {Kind: KindNone, SocketPath: "/tmp/v/s"}} {
		if got, err := rt.BindEndpointEnv(base); !errors.Is(err, ErrContainerSpec) || got != nil {
			t.Errorf("%+v: got %q, %v; want ErrContainerSpec", rt, got, err)
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
	if err == nil || !strings.Contains(err.Error(), "forbidden root") || !strings.Contains(err.Error(), "/var/run") {
		t.Fatalf("err = %v", err)
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
		"GOTOOLCHAIN=local", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
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
