// Package gateiso carries the pure pieces of the ADR-063 (#2127 / #2134)
// gate-isolation layer: safe container-runtime detection over the EFFECTIVE
// endpoint, the mode × profile × runtime selection policy, and the container
// argv/env builder with its resolved-path socket-mount guard. The runner wires
// them at its one gate-exec seam (runBoundedGateArgv); nothing here executes a
// gate itself.
//
// The runtime detector classifies the endpoint the docker/podman CLI would
// actually talk to, not merely whether a binary is on PATH: a remote context
// (tcp/ssh/npipe), an unresolvable or non-socket node, an unreachable socket,
// or a runner that is itself inside a container (docker-outside-of-docker,
// where the "isolated" container is a sibling on the host daemon and a mounted
// checkout would land on the host) are all UNSAFE with a named reason, so the
// selection policy falls back or refuses instead of executing untrusted gate
// commands against a daemon it cannot vouch for.
package gateiso

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Kind names a detected container runtime.
type Kind string

// Runtime kinds. KindNone means no runtime binary was found on PATH.
const (
	KindNone   Kind = "none"
	KindDocker Kind = "docker"
	KindPodman Kind = "podman"
)

// Binary returns the CLI executable name for the kind ("" for KindNone).
func (k Kind) Binary() string {
	switch k {
	case KindDocker, KindPodman:
		return string(k)
	}
	return ""
}

// Probes is the injectable host surface DetectRuntime and ClassifyEndpoint
// read through. DefaultProbes wires the real host; tests substitute fakes.
type Probes struct {
	// LookPath resolves a binary on PATH (exec.LookPath).
	LookPath func(file string) (string, error)
	// Run executes a CLI probe and returns its trimmed stdout.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// FileExists reports whether a path exists (any node type).
	FileExists func(path string) bool
	// ReadFile reads a whole file (os.ReadFile).
	ReadFile func(path string) ([]byte, error)
	// Getenv reads an environment variable (os.Getenv).
	Getenv func(key string) string
	// EvalSymlinks resolves every symlink in a path (filepath.EvalSymlinks).
	EvalSymlinks func(path string) (string, error)
	// Lstat stats a path WITHOUT following a final symlink (os.Lstat).
	Lstat func(path string) (os.FileInfo, error)
	// DialUnix connects to a unix socket path and closes the connection.
	DialUnix func(path string) error
	// Getuid returns the runner's numeric uid (os.Getuid).
	Getuid func() int
	// GOOS is the host operating system (runtime.GOOS).
	GOOS string
}

// probeTimeout bounds each CLI probe so a wedged daemon cannot stall runner
// startup: a probe that does not answer is classified UNSAFE, not awaited.
const probeTimeout = 15 * time.Second

// DefaultProbes returns Probes bound to the real host.
func DefaultProbes() Probes {
	return Probes{
		LookPath: exec.LookPath,
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) && len(ee.Stderr) > 0 {
					return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
				}
				return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
			}
			return strings.TrimSpace(string(out)), nil
		},
		FileExists: func(path string) bool {
			_, err := os.Lstat(path)
			return err == nil
		},
		ReadFile:     os.ReadFile,
		Getenv:       os.Getenv,
		EvalSymlinks: filepath.EvalSymlinks,
		Lstat:        os.Lstat,
		DialUnix: func(path string) error {
			c, err := net.DialTimeout("unix", path, 2*time.Second)
			if err != nil {
				return err
			}
			return c.Close()
		},
		Getuid: os.Getuid,
		GOOS:   runtime.GOOS,
	}
}

// Endpoint is the classification of one daemon endpoint string.
type Endpoint struct {
	// Raw is the endpoint as configured (DOCKER_HOST, a context's
	// Endpoints.docker.Host, CONTAINER_HOST, or a podman connection URI).
	Raw string `json:"raw"`
	// Scheme is the parsed scheme ("unix", "tcp", "ssh", "npipe", …);
	// a bare absolute path is reported as "unix".
	Scheme string `json:"scheme"`
	// Path is the unix socket path before symlink resolution ("" unless
	// Scheme is unix).
	Path string `json:"path,omitempty"`
	// Resolved is Path after EvalSymlinks ("" when unresolvable).
	Resolved string `json:"resolved,omitempty"`
	// Local is true only when the endpoint is a local, existing, dialable
	// unix socket.
	Local bool `json:"local"`
	// Reason names why Local is false ("" when Local).
	Reason string `json:"reason,omitempty"`
}

// ClassifyEndpoint decides whether raw names a LOCAL daemon endpoint: the
// scheme must be unix (or raw a bare absolute path), the path must resolve
// through symlinks, Lstat must report a socket node, and a dial must succeed.
// tcp/ssh/npipe/fd/http/unparsable/relative/missing/non-socket/refused all
// classify as not local, naming why.
func ClassifyEndpoint(p Probes, raw string) Endpoint {
	ep := Endpoint{Raw: raw}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		ep.Reason = "empty endpoint"
		return ep
	}
	var path string
	switch {
	case strings.HasPrefix(raw, "/"):
		ep.Scheme = "unix"
		path = raw
	default:
		i := strings.Index(raw, "://")
		if i <= 0 {
			ep.Reason = fmt.Sprintf("unparsable endpoint %q (expected unix:///path or an absolute path)", raw)
			return ep
		}
		ep.Scheme = strings.ToLower(raw[:i])
		if ep.Scheme != "unix" {
			ep.Reason = fmt.Sprintf("endpoint %q uses scheme %s, not a local unix socket", raw, ep.Scheme)
			return ep
		}
		path = raw[i+len("://"):]
	}
	if !strings.HasPrefix(path, "/") {
		ep.Reason = fmt.Sprintf("unix socket path %q is not absolute", path)
		return ep
	}
	ep.Path = path
	resolved, err := p.EvalSymlinks(path)
	if err != nil {
		ep.Reason = fmt.Sprintf("unix socket path %q is unresolvable: %v", path, err)
		return ep
	}
	ep.Resolved = resolved
	fi, err := p.Lstat(resolved)
	if err != nil {
		ep.Reason = fmt.Sprintf("unix socket path %q is missing: %v", resolved, err)
		return ep
	}
	if fi.Mode()&os.ModeSocket == 0 {
		ep.Reason = fmt.Sprintf("path %q is not a unix socket (mode %s)", resolved, fi.Mode().Type())
		return ep
	}
	if err := p.DialUnix(resolved); err != nil {
		ep.Reason = fmt.Sprintf("unix socket %q is unreachable: %v", resolved, err)
		return ep
	}
	ep.Local = true
	return ep
}

// Runtime is DetectRuntime's verdict. Safe is the single bit the selection
// policy reads; every other field is evidence recorded alongside it.
type Runtime struct {
	Kind Kind `json:"kind"`
	// Safe is true only when the effective endpoint is a local dialable unix
	// socket and the runner is not itself inside a container.
	Safe bool `json:"safe"`
	// Reason names why Safe is false, or summarises the safe verdict.
	Reason string `json:"reason"`
	// Rootless reports podman's Host.Security.Rootless (false for docker).
	Rootless bool `json:"rootless"`
	// RunnerInContainer reports whether the runner process is itself
	// containerised (/.dockerenv, /run/.containerenv, /proc/1/cgroup).
	RunnerInContainer bool `json:"runner_in_container"`
	// Version is the daemon/CLI version string ("" when not probed).
	Version string `json:"version,omitempty"`
	// Endpoint is the classified effective endpoint.
	Endpoint Endpoint `json:"endpoint"`
	// SocketPath is Endpoint.Resolved when Safe: the daemon socket the mount
	// guard must refuse inside any bind-mount source.
	SocketPath string `json:"socket_path,omitempty"`
}

// DetectRuntime classifies the container runtime the runner could use. Docker
// is evaluated first, then podman; the first SAFE verdict wins, otherwise the
// first evaluated verdict is returned (so the reason names the primary
// runtime's defect), and KindNone when neither binary is on PATH.
func DetectRuntime(ctx context.Context, p Probes) Runtime {
	inContainer := runnerInContainer(p)
	var first *Runtime
	if _, err := p.LookPath("docker"); err == nil {
		rt := detectDocker(ctx, p, inContainer)
		if rt.Safe {
			return rt
		}
		first = &rt
	}
	if _, err := p.LookPath("podman"); err == nil {
		rt := detectPodman(ctx, p, inContainer)
		if rt.Safe {
			return rt
		}
		if first == nil {
			first = &rt
		}
	}
	if first != nil {
		return *first
	}
	return Runtime{Kind: KindNone, Reason: "no container runtime on PATH (docker or podman)", RunnerInContainer: inContainer}
}

// runnerInContainer reports whether the runner process is itself inside a
// container. A safe runtime is a HOST daemon; from inside a container the
// daemon socket would be the host's (docker-outside-of-docker), so a gate
// container would be a sibling on the host and the mounted checkout path
// would be interpreted in the host's filesystem namespace.
func runnerInContainer(p Probes) bool {
	if p.FileExists("/.dockerenv") || p.FileExists("/run/.containerenv") {
		return true
	}
	if p.GOOS != "linux" {
		return false
	}
	b, err := p.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	for _, marker := range []string{"/docker/", "/docker-", "/containerd", "/kubepods", "/libpod-", "/podman"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func unsafe(rt Runtime, reason string) Runtime {
	rt.Safe = false
	rt.Reason = reason
	rt.SocketPath = ""
	return rt
}

// detectDocker classifies the docker CLI's effective endpoint. Per the docker
// CLI's documented precedence DOCKER_CONTEXT selects the context and
// DOCKER_HOST, when set, overrides the endpoint; rather than encode the
// precedence, EVERY candidate (DOCKER_HOST if set AND the active context's
// Endpoints.docker.Host) must classify Local — either remote → UNSAFE.
func detectDocker(ctx context.Context, p Probes, inContainer bool) Runtime {
	rt := Runtime{Kind: KindDocker, RunnerInContainer: inContainer}
	ctxName := strings.TrimSpace(p.Getenv("DOCKER_CONTEXT"))
	if ctxName == "" {
		out, err := p.Run(ctx, "docker", "context", "show")
		if err != nil {
			return unsafe(rt, fmt.Sprintf("cannot resolve the active docker context: %v", err))
		}
		ctxName = strings.TrimSpace(out)
	}
	if ctxName == "" {
		return unsafe(rt, "docker context show returned an empty context name")
	}
	ctxHost, err := p.Run(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}", ctxName)
	if err != nil {
		return unsafe(rt, fmt.Sprintf("cannot inspect docker context %q endpoint: %v", ctxName, err))
	}
	type candidate struct{ source, raw string }
	var cands []candidate
	if h := strings.TrimSpace(p.Getenv("DOCKER_HOST")); h != "" {
		cands = append(cands, candidate{"DOCKER_HOST", h})
	}
	cands = append(cands, candidate{fmt.Sprintf("docker context %q", ctxName), strings.TrimSpace(ctxHost)})
	for _, c := range cands {
		ep := ClassifyEndpoint(p, c.raw)
		rt.Endpoint = ep
		if !ep.Local {
			return unsafe(rt, fmt.Sprintf("docker endpoint from %s is not a local unix socket: %s", c.source, ep.Reason))
		}
	}
	if inContainer {
		return unsafe(rt, "runner is itself inside a container: the docker socket belongs to the host daemon (docker-outside-of-docker), so a gate container would not be isolated from the host")
	}
	ver, err := p.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return unsafe(rt, fmt.Sprintf("docker daemon did not answer a version probe: %v", err))
	}
	rt.Version = strings.TrimSpace(ver)
	rt.Safe = true
	rt.SocketPath = rt.Endpoint.Resolved
	rt.Reason = fmt.Sprintf("docker %s over local unix socket %s", rt.Version, rt.SocketPath)
	return rt
}

// detectPodman classifies podman: Host.RemoteSocket.Exists and
// Host.Security.Rootless must both be true, and the connection podman would
// use (CONTAINER_HOST, else the `podman system connection list` row named by
// CONTAINER_CONNECTION or flagged Default) must classify Local. A podman
// machine (ssh://), a missing connection, or a stopped socket service is
// UNSAFE; the remedy for the last is
// `systemctl --user enable --now podman.socket`.
func detectPodman(ctx context.Context, p Probes, inContainer bool) Runtime {
	rt := Runtime{Kind: KindPodman, RunnerInContainer: inContainer}
	info, err := p.Run(ctx, "podman", "info", "--format", "{{.Host.RemoteSocket.Exists}} {{.Host.Security.Rootless}}")
	if err != nil {
		return unsafe(rt, fmt.Sprintf("podman info failed: %v", err))
	}
	fields := strings.Fields(info)
	if len(fields) != 2 {
		return unsafe(rt, fmt.Sprintf("podman info returned %q, expected \"<remote-socket-exists> <rootless>\"", info))
	}
	rt.Rootless = fields[1] == "true"
	if fields[0] != "true" {
		return unsafe(rt, "podman remote socket is not running (Host.RemoteSocket.Exists=false); enable it with `systemctl --user enable --now podman.socket`")
	}
	if !rt.Rootless {
		return unsafe(rt, "podman is rootful (Host.Security.Rootless=false); only a rootless podman is a safe gate runtime")
	}
	raw := strings.TrimSpace(p.Getenv("CONTAINER_HOST"))
	source := "CONTAINER_HOST"
	if raw == "" {
		rows, err := p.Run(ctx, "podman", "system", "connection", "list", "--format", "{{.Name}} {{.URI}} {{.Default}}")
		if err != nil {
			return unsafe(rt, fmt.Sprintf("podman system connection list failed: %v", err))
		}
		want := strings.TrimSpace(p.Getenv("CONTAINER_CONNECTION"))
		for _, line := range strings.Split(rows, "\n") {
			f := strings.Fields(line)
			if len(f) != 3 {
				continue
			}
			if (want != "" && f[0] == want) || (want == "" && f[2] == "true") {
				raw = f[1]
				source = fmt.Sprintf("podman connection %q", f[0])
				break
			}
		}
		if raw == "" {
			if want != "" {
				return unsafe(rt, fmt.Sprintf("podman connection %q (CONTAINER_CONNECTION) not found", want))
			}
			return unsafe(rt, "podman has no default system connection and CONTAINER_HOST is unset")
		}
	}
	ep := ClassifyEndpoint(p, raw)
	rt.Endpoint = ep
	if !ep.Local {
		return unsafe(rt, fmt.Sprintf("podman endpoint from %s is not a local unix socket: %s", source, ep.Reason))
	}
	if inContainer {
		return unsafe(rt, "runner is itself inside a container: the podman socket belongs to the host, so a gate container would not be isolated from the host")
	}
	ver, err := p.Run(ctx, "podman", "version", "--format", "{{.Client.Version}}")
	if err != nil {
		return unsafe(rt, fmt.Sprintf("podman did not answer a version probe: %v", err))
	}
	rt.Version = strings.TrimSpace(ver)
	rt.Safe = true
	rt.SocketPath = ep.Resolved
	rt.Reason = fmt.Sprintf("rootless podman %s over local unix socket %s", rt.Version, rt.SocketPath)
	return rt
}
