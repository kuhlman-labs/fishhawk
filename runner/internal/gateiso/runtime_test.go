package gateiso

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortTempDir returns a temp dir short enough for a unix socket path
// (macOS caps sun_path at 104 bytes; t.TempDir() paths under /var/folders
// routinely exceed it).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gi")
	if err != nil {
		dir = t.TempDir()
	} else {
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	return dir
}

// listenUnix creates a real, dialable unix socket and keeps it listening for
// the test's lifetime.
func listenUnix(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { l.Close() })
	return path
}

// deadSocket leaves a socket NODE on disk with nothing listening behind it.
func deadSocket(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	return path
}

type fakeHost struct {
	bins  map[string]bool
	cmds  map[string]string
	env   map[string]string
	files map[string]bool
	read  map[string]string
	goos  string
	calls []string
}

func (f *fakeHost) probes() Probes {
	p := DefaultProbes()
	p.LookPath = func(file string) (string, error) {
		if f.bins[file] {
			return "/usr/bin/" + file, nil
		}
		return "", errors.New("not found")
	}
	p.Run = func(_ context.Context, name string, args ...string) (string, error) {
		key := name + " " + strings.Join(args, " ")
		f.calls = append(f.calls, key)
		out, ok := f.cmds[key]
		if !ok {
			return "", errors.New("fake: no stub for " + key)
		}
		return out, nil
	}
	p.Getenv = func(k string) string { return f.env[k] }
	p.FileExists = func(path string) bool { return f.files[path] }
	p.ReadFile = func(path string) ([]byte, error) {
		s, ok := f.read[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(s), nil
	}
	if f.goos != "" {
		p.GOOS = f.goos
	}
	return p
}

const (
	cmdCtxShow     = "docker context show"
	cmdDockerVer   = "docker version --format {{.Server.Version}}"
	cmdPodmanInfo  = "podman info --format {{.Host.RemoteSocket.Exists}} {{.Host.Security.Rootless}}"
	cmdPodmanList  = "podman system connection list --format {{.Name}} {{.URI}} {{.Default}}"
	cmdPodmanVer   = "podman version --format {{.Client.Version}}"
	cmdCtxInspectP = "docker context inspect --format {{.Endpoints.docker.Host}} "
)

func dockerHost(ctxName, ctxHost string) *fakeHost {
	return &fakeHost{
		bins: map[string]bool{"docker": true},
		cmds: map[string]string{
			cmdCtxShow:               ctxName,
			cmdCtxInspectP + ctxName: ctxHost,
			cmdDockerVer:             "27.0.1",
		},
		env:   map[string]string{},
		files: map[string]bool{},
	}
}

func podmanHost(list string) *fakeHost {
	return &fakeHost{
		bins: map[string]bool{"podman": true},
		cmds: map[string]string{
			cmdPodmanInfo: "true true",
			cmdPodmanList: list,
			cmdPodmanVer:  "5.2.1",
		},
		env:   map[string]string{},
		files: map[string]bool{},
	}
}

func mustUnsafe(t *testing.T, rt Runtime, kind Kind, reasonParts ...string) {
	t.Helper()
	if rt.Safe {
		t.Fatalf("expected UNSAFE, got safe: %+v", rt)
	}
	if rt.Kind != kind {
		t.Fatalf("kind = %q, want %q (%+v)", rt.Kind, kind, rt)
	}
	for _, part := range reasonParts {
		if !strings.Contains(rt.Reason, part) {
			t.Fatalf("reason %q does not name %q", rt.Reason, part)
		}
	}
	if rt.SocketPath != "" {
		t.Fatalf("unsafe verdict must not carry a SocketPath, got %q", rt.SocketPath)
	}
}

func mustSafe(t *testing.T, rt Runtime, kind Kind, sock string) {
	t.Helper()
	if !rt.Safe {
		t.Fatalf("expected SAFE, got: %+v", rt)
	}
	if rt.Kind != kind {
		t.Fatalf("kind = %q, want %q", rt.Kind, kind)
	}
	want, err := filepath.EvalSymlinks(sock)
	if err != nil {
		t.Fatal(err)
	}
	if rt.SocketPath != want {
		t.Fatalf("SocketPath = %q, want %q", rt.SocketPath, want)
	}
	if !rt.Endpoint.Local || rt.Endpoint.Resolved != want {
		t.Fatalf("endpoint not local/resolved: %+v", rt.Endpoint)
	}
	if rt.Version == "" {
		t.Fatalf("version not recorded: %+v", rt)
	}
}

func TestDetectRuntime_RemoteContextWithoutDockerHostIsUnsafe(t *testing.T) {
	h := dockerHost("remote", "tcp://10.0.0.5:2376")
	rt := DetectRuntime(context.Background(), h.probes())
	mustUnsafe(t, rt, KindDocker, `docker context "remote"`, "scheme tcp")
	if rt.Endpoint.Scheme != "tcp" || rt.Endpoint.Local {
		t.Fatalf("endpoint = %+v", rt.Endpoint)
	}
}

func TestDetectRuntime_DockerHostRemoteIsUnsafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "d.sock")
	for _, raw := range []string{"ssh://core@build-host", "tcp://1.2.3.4:2375", "npipe:////./pipe/docker_engine", "fd://"} {
		t.Run(raw, func(t *testing.T) {
			h := dockerHost("default", "unix://"+sock)
			h.env["DOCKER_HOST"] = raw
			rt := DetectRuntime(context.Background(), h.probes())
			scheme := raw[:strings.Index(raw, "://")]
			mustUnsafe(t, rt, KindDocker, "DOCKER_HOST", "scheme "+scheme)
		})
	}
}

func TestDetectRuntime_BothCandidatesMustBeLocal(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "d.sock")
	t.Run("local DOCKER_HOST, remote context", func(t *testing.T) {
		h := dockerHost("remote", "tcp://10.0.0.5:2376")
		h.env["DOCKER_HOST"] = "unix://" + sock
		mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, `docker context "remote"`, "tcp")
	})
	t.Run("remote DOCKER_HOST, local context", func(t *testing.T) {
		h := dockerHost("default", "unix://"+sock)
		h.env["DOCKER_HOST"] = "tcp://10.0.0.5:2376"
		mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, "DOCKER_HOST", "tcp")
	})
	t.Run("both local", func(t *testing.T) {
		h := dockerHost("default", "unix://"+sock)
		h.env["DOCKER_HOST"] = "unix://" + sock
		mustSafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, sock)
	})
}

func TestDetectRuntime_DockerContextEnvSelectsContext(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "d.sock")
	h := dockerHost("default", "tcp://10.0.0.5:2376")
	h.cmds[cmdCtxInspectP+"alt"] = "unix://" + sock
	h.env["DOCKER_CONTEXT"] = "alt"
	delete(h.cmds, cmdCtxShow) // must not be consulted when DOCKER_CONTEXT is set
	mustSafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, sock)
	for _, c := range h.calls {
		if c == cmdCtxShow {
			t.Fatalf("docker context show consulted despite DOCKER_CONTEXT")
		}
	}
}

func TestDetectRuntime_NonSocketNodeIsUnsafe(t *testing.T) {
	dir := shortTempDir(t)
	reg := filepath.Join(dir, "docker.sock")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := dockerHost("default", "unix://"+reg)
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, "not a unix socket")
}

func TestDetectRuntime_MissingSocketIsUnsafe(t *testing.T) {
	h := dockerHost("default", "unix:///nonexistent/gateiso/docker.sock")
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, "unresolvable")
}

func TestDetectRuntime_UnreachableSocketIsUnsafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := deadSocket(t, dir, "dead.sock")
	h := dockerHost("default", "unix://"+sock)
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, "unreachable")
}

func TestDetectRuntime_LocalSocketIsSafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "d.sock")
	h := dockerHost("desktop-linux", "unix://"+sock)
	rt := DetectRuntime(context.Background(), h.probes())
	mustSafe(t, rt, KindDocker, sock)
	if rt.Version != "27.0.1" || rt.RunnerInContainer || rt.Rootless {
		t.Fatalf("verdict = %+v", rt)
	}
	if !strings.Contains(rt.Reason, "local unix socket") {
		t.Fatalf("reason = %q", rt.Reason)
	}
}

func TestDetectRuntime_ContextShowFailureIsUnsafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "d.sock")
	h := dockerHost("default", "unix://"+sock)
	delete(h.cmds, cmdCtxShow)
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, "cannot resolve the active docker context")
}

func TestDetectRuntime_VersionProbeFailureIsUnsafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "d.sock")
	h := dockerHost("default", "unix://"+sock)
	delete(h.cmds, cmdDockerVer)
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, "version probe")
}

func TestDetectRuntime_DockerOutsideOfDockerIsUnsafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "d.sock")
	cases := map[string]func(h *fakeHost){
		"dockerenv":    func(h *fakeHost) { h.files["/.dockerenv"] = true },
		"containerenv": func(h *fakeHost) { h.files["/run/.containerenv"] = true },
		"cgroup": func(h *fakeHost) {
			h.goos = "linux"
			h.read = map[string]string{"/proc/1/cgroup": "0::/system.slice/docker-abc123.scope\n"}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			h := dockerHost("default", "unix://"+sock)
			plant(h)
			rt := DetectRuntime(context.Background(), h.probes())
			mustUnsafe(t, rt, KindDocker, "inside a container")
			if !rt.RunnerInContainer {
				t.Fatalf("RunnerInContainer not recorded: %+v", rt)
			}
		})
	}
	t.Run("cgroup v2 plain host is not a container", func(t *testing.T) {
		h := dockerHost("default", "unix://"+sock)
		h.goos = "linux"
		h.read = map[string]string{"/proc/1/cgroup": "0::/\n"}
		mustSafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, sock)
	})
}

func TestDetectRuntime_PodmanRemoteSocketAbsent(t *testing.T) {
	h := podmanHost("")
	h.cmds[cmdPodmanInfo] = "false true"
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, "podman.socket")
}

func TestDetectRuntime_PodmanRootfulIsUnsafe(t *testing.T) {
	h := podmanHost("")
	h.cmds[cmdPodmanInfo] = "true false"
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, "rootful")
}

func TestDetectRuntime_PodmanSSHConnectionIsUnsafe(t *testing.T) {
	h := podmanHost("podman-machine-default ssh://core@127.0.0.1:50123/run/user/501/podman/podman.sock true")
	rt := DetectRuntime(context.Background(), h.probes())
	mustUnsafe(t, rt, KindPodman, `podman connection "podman-machine-default"`, "scheme ssh")
}

func TestDetectRuntime_PodmanNoConnectionIsUnsafe(t *testing.T) {
	h := podmanHost("other unix:///x/y.sock false")
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, "no default system connection")
}

func TestDetectRuntime_PodmanRootlessLocalSocketIsSafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "p.sock")
	h := podmanHost("local unix://" + sock + " true")
	rt := DetectRuntime(context.Background(), h.probes())
	mustSafe(t, rt, KindPodman, sock)
	if !rt.Rootless || rt.Version != "5.2.1" {
		t.Fatalf("verdict = %+v", rt)
	}
}

func TestDetectRuntime_PodmanInContainerIsUnsafe(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "p.sock")
	h := podmanHost("local unix://" + sock + " true")
	h.files["/run/.containerenv"] = true
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, "inside a container")
}

func TestDetectRuntime_PodmanContainerHostOverride(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "p.sock")
	h := podmanHost("machine ssh://core@127.0.0.1:1/x.sock true")
	h.env["CONTAINER_HOST"] = "unix://" + sock
	delete(h.cmds, cmdPodmanList)
	mustSafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, sock)
	h2 := podmanHost("local unix://" + sock + " true")
	h2.env["CONTAINER_HOST"] = "ssh://core@127.0.0.1:1/x.sock"
	mustUnsafe(t, DetectRuntime(context.Background(), h2.probes()), KindPodman, "CONTAINER_HOST", "scheme ssh")
}

func TestDetectRuntime_PodmanContainerConnectionSelectsRow(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "p.sock")
	list := "machine ssh://core@127.0.0.1:1/x.sock true\nlocal unix://" + sock + " false"
	h := podmanHost(list)
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, `"machine"`, "scheme ssh")
	h.env["CONTAINER_CONNECTION"] = "local"
	mustSafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, sock)
	h.env["CONTAINER_CONNECTION"] = "absent"
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, `"absent"`, "not found")
}

func TestDetectRuntime_NoneWhenNoBinary(t *testing.T) {
	h := &fakeHost{bins: map[string]bool{}, cmds: map[string]string{}, env: map[string]string{}, files: map[string]bool{}}
	rt := DetectRuntime(context.Background(), h.probes())
	mustUnsafe(t, rt, KindNone, "no container runtime on PATH")
}

func TestDetectRuntime_SafePodmanWinsOverUnsafeDocker(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "p.sock")
	h := podmanHost("local unix://" + sock + " true")
	h.bins["docker"] = true
	h.cmds[cmdCtxShow] = "remote"
	h.cmds[cmdCtxInspectP+"remote"] = "tcp://10.0.0.5:2376"
	h.cmds[cmdDockerVer] = "27.0.1"
	mustSafe(t, DetectRuntime(context.Background(), h.probes()), KindPodman, sock)
	// Both unsafe → docker's (the first evaluated) reason is reported.
	h.cmds[cmdPodmanInfo] = "false true"
	mustUnsafe(t, DetectRuntime(context.Background(), h.probes()), KindDocker, "tcp")
}

func TestClassifyEndpoint_Table(t *testing.T) {
	dir := shortTempDir(t)
	sock := listenUnix(t, dir, "e.sock")
	p := DefaultProbes()
	cases := []struct {
		raw    string
		local  bool
		scheme string
		reason string
	}{
		{"", false, "", "empty endpoint"},
		{"garbage", false, "", "unparsable"},
		{"unix://relative/path.sock", false, "unix", "not absolute"},
		{"http://localhost:2375", false, "http", "scheme http"},
		{"UNIX://" + sock, true, "unix", ""},
		{sock, true, "unix", ""},
		{"unix://" + sock, true, "unix", ""},
	}
	for _, c := range cases {
		ep := ClassifyEndpoint(p, c.raw)
		if ep.Local != c.local || ep.Scheme != c.scheme || !strings.Contains(ep.Reason, c.reason) {
			t.Errorf("ClassifyEndpoint(%q) = %+v, want local=%v scheme=%q reason~%q", c.raw, ep, c.local, c.scheme, c.reason)
		}
	}
}

func TestDefaultProbes_RunTrimsAndReportsFailure(t *testing.T) {
	p := DefaultProbes()
	out, err := p.Run(context.Background(), "sh", "-c", "echo '  hello  '")
	if err != nil || out != "hello" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if _, err := p.Run(context.Background(), "sh", "-c", "echo boom >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected stderr-bearing error, got %v", err)
	}
	if p.GOOS == "" || p.Getuid() < 0 {
		t.Fatal("default probes incomplete")
	}
	if !p.FileExists(os.Args[0]) || p.FileExists("/nonexistent/gateiso") {
		t.Fatal("FileExists probe wrong")
	}
}
