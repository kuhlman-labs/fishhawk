package gateiso

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// In-container mount points. The checkout is the working directory; the three
// caches are per-exec throwaway directories (cache.go) so nothing the gate
// writes outlives the exec.
const (
	MountWork       = "/work"
	MountGoCache    = "/gocache"
	MountGoModCache = "/gomodcache"
	MountLintCache  = "/lintcache"
)

// DefaultMaxMountDepth bounds the socket walk under each bind-mount source.
// A socket deeper than this is NOT detected — a documented, test-pinned limit
// (TestForbidSocketMounts_DepthBound), not a control.
const DefaultMaxMountDepth = 8

// ContainerSpec describes one gate container exec. BuildArgv renders it.
type ContainerSpec struct {
	Runtime Runtime
	Image   string
	// Name is the container name (NewContainerName); KillArgv removes it.
	Name string
	// Checkout, GoCache, GoModCache, LintCache are the four host-side
	// bind-mount sources, each guarded by ForbidSocketMounts.
	Checkout   string
	GoCache    string
	GoModCache string
	LintCache  string
	// UID/GID own the bind-mount writes (--user uid:gid; rootless podman uses
	// --userns=keep-id instead).
	UID int
	GID int
	// Env is the in-container environment (ContainerEnv), passed via -e.
	Env []string
	// Argv is the gate command executed after `--entrypoint ''` resets the
	// image's ENTRYPOINT.
	Argv []string
}

// NewContainerName returns a fresh, runtime-legal container name.
func NewContainerName() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("gateiso: crypto/rand unavailable: " + err.Error())
	}
	return "fishhawk-gate-" + hex.EncodeToString(b[:])
}

// MountPolicy parameterises ForbidSocketMounts.
type MountPolicy struct {
	// Permitted are the roots a SYMLINKED source may resolve into; a source
	// whose resolved path differs from its literal path and lies outside
	// every permitted root is refused.
	Permitted []string
	// DaemonSocket is the detected runtime socket (Runtime.SocketPath); a
	// source equal to or containing it is refused.
	DaemonSocket string
	// MaxDepth bounds the socket walk (0 → DefaultMaxMountDepth).
	MaxDepth int
}

// forbiddenRoots are refused as resolved mount prefixes: daemon sockets and
// other runtime state live here on every supported host (macOS /var/run
// resolves to /private/var/run and is matched after resolution).
var forbiddenRoots = []string{"/run", "/var/run"}

// ForbidSocketMounts refuses any bind-mount source that could hand the gate
// container a daemon socket. Every source is resolved with EvalSymlinks
// first, so an innocuously named symlink cannot dodge the checks; a source is
// refused when it is empty or relative, unresolvable, resolved under /run or
// /var/run, a symlink resolving outside policy.Permitted, itself a socket,
// equal to or containing policy.DaemonSocket, or a directory containing a
// socket (a symlink whose target is a socket counts) within policy.MaxDepth.
// The walk never follows symlinks into other trees.
func ForbidSocketMounts(policy MountPolicy, sources ...string) error {
	maxDepth := policy.MaxDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxMountDepth
	}
	forbidden := resolvedRoots(forbiddenRoots)
	permitted := resolvedRoots(policy.Permitted)
	var daemon string
	if policy.DaemonSocket != "" {
		daemon = policy.DaemonSocket
		if r, err := filepath.EvalSymlinks(policy.DaemonSocket); err == nil {
			daemon = r
		}
	}
	for _, src := range sources {
		if src == "" || !filepath.IsAbs(src) {
			return fmt.Errorf("mount source %q refused: must be an absolute path", src)
		}
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			return fmt.Errorf("mount source %q refused: unresolvable: %w", src, err)
		}
		for _, root := range forbidden {
			if underRoot(resolved, root) {
				return fmt.Errorf("mount source %q refused: resolves to %q under forbidden root %q", src, resolved, root)
			}
		}
		if resolved != filepath.Clean(src) {
			ok := false
			for _, root := range permitted {
				if underRoot(resolved, root) {
					ok = true
					break
				}
			}
			if !ok {
				return fmt.Errorf("mount source %q refused: symlink resolves to %q outside the permitted roots %v", src, resolved, policy.Permitted)
			}
		}
		fi, err := os.Lstat(resolved)
		if err != nil {
			return fmt.Errorf("mount source %q refused: cannot stat %q: %w", src, resolved, err)
		}
		if fi.Mode()&os.ModeSocket != 0 {
			return fmt.Errorf("mount source %q refused: %q is a unix socket", src, resolved)
		}
		if daemon != "" && (resolved == daemon || underRoot(daemon, resolved)) {
			return fmt.Errorf("mount source %q refused: contains the runtime daemon socket %q", src, daemon)
		}
		if !fi.IsDir() {
			continue
		}
		if err := walkForSockets(resolved, maxDepth); err != nil {
			return fmt.Errorf("mount source %q refused: %w", src, err)
		}
	}
	return nil
}

// walkForSockets refuses the first socket found under root within maxDepth
// components. Directory entries at maxDepth are not descended; symlink
// entries are Stat'd (following) to catch a link to a socket but are never
// descended into.
func walkForSockets(root string, maxDepth int) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("cannot walk %q for sockets: %w", path, err)
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		t := d.Type()
		switch {
		case t&os.ModeSocket != 0:
			return fmt.Errorf("%q is a unix socket (depth %d)", path, depth)
		case t&os.ModeSymlink != 0:
			if fi, serr := os.Stat(path); serr == nil && fi.Mode()&os.ModeSocket != 0 {
				return fmt.Errorf("%q is a symlink to unix socket %q (depth %d)", path, fi.Name(), depth)
			}
			return nil
		case d.IsDir():
			if depth >= maxDepth {
				return filepath.SkipDir
			}
		}
		return nil
	})
}

// resolvedRoots EvalSymlinks each root, keeping the literal when unresolvable
// (a missing /var/run on a host without it matches nothing).
func resolvedRoots(roots []string) []string {
	out := make([]string, 0, len(roots)*2)
	for _, r := range roots {
		if r == "" {
			continue
		}
		out = append(out, filepath.Clean(r))
		if res, err := filepath.EvalSymlinks(r); err == nil && res != filepath.Clean(r) {
			out = append(out, res)
		}
	}
	return out
}

// underRoot reports whether path equals root or lies beneath it by PATH
// COMPONENT (so /runway is not under /run).
func underRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	if root == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// ErrContainerSpec is wrapped by BuildArgv for an incomplete spec.
var ErrContainerSpec = errors.New("incomplete container spec")

// Endpoint binding. DetectRuntime validates the endpoint the CLI would talk
// to at SELECTION time, but the CLI re-resolves its endpoint on EVERY
// invocation from mutable state — the active docker context
// (~/.docker/config.json, `docker context use`), DOCKER_HOST/DOCKER_CONTEXT,
// CONTAINER_HOST/CONTAINER_CONNECTION, containers.conf — so a context switch
// between two gates would otherwise send the next bind-mount request to a
// daemon nobody validated. Every runtime invocation the runner builds
// therefore BINDS the validated socket explicitly, in the argv (EndpointArgs:
// `docker --host unix://<socket>` / `podman --url unix://<socket>`, the
// global flag that overrides the context store and the environment) AND in
// the exec environment (BindEndpointEnv: the override variables dropped and
// DOCKER_HOST / CONTAINER_HOST re-pinned to the same socket). A Runtime with
// no validated SocketPath cannot be rendered at all.

// endpointOverrideVars are the environment variables through which the
// docker / podman CLI's effective endpoint can be redirected; BindEndpointEnv
// drops every one of them before re-pinning the validated socket.
var endpointOverrideVars = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "CONTAINER_HOST", "CONTAINER_CONNECTION"}

// EndpointArgs returns the runtime CLI's global endpoint flag bound to the
// validated socket: the tokens go between the binary and the subcommand. It
// fails (ErrContainerSpec) for an unsupported kind or an empty SocketPath.
func (r Runtime) EndpointArgs() ([]string, error) {
	if r.SocketPath == "" {
		return nil, fmt.Errorf("%w: runtime %q has no validated socket path to bind", ErrContainerSpec, r.Kind)
	}
	switch r.Kind {
	case KindDocker:
		return []string{"--host", "unix://" + r.SocketPath}, nil
	case KindPodman:
		return []string{"--url", "unix://" + r.SocketPath}, nil
	}
	return nil, fmt.Errorf("%w: runtime kind %q", ErrContainerSpec, r.Kind)
}

// BindEndpointEnv returns base with every endpoint override variable removed
// and the validated socket re-pinned (DOCKER_HOST for docker, CONTAINER_HOST
// for podman). It never reads the process environment; the caller supplies
// base (the runtime CLI needs PATH and its config dir from it).
func (r Runtime) BindEndpointEnv(base []string) ([]string, error) {
	if r.SocketPath == "" {
		return nil, fmt.Errorf("%w: runtime %q has no validated socket path to bind", ErrContainerSpec, r.Kind)
	}
	var pin string
	switch r.Kind {
	case KindDocker:
		pin = "DOCKER_HOST=unix://" + r.SocketPath
	case KindPodman:
		pin = "CONTAINER_HOST=unix://" + r.SocketPath
	default:
		return nil, fmt.Errorf("%w: runtime kind %q", ErrContainerSpec, r.Kind)
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, v := range endpointOverrideVars {
			if k == v {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, pin), nil
}

// BuildArgv renders the runtime command line. It applies ForbidSocketMounts
// to every bind-mount source BEFORE emitting any -v token and returns the
// refusal with no argv. The exact shape is:
//
//	<runtime> run --rm --name <name> --network=none --cap-drop=ALL
//	  --security-opt=no-new-privileges --workdir /work
//	  (--user uid:gid | --userns=keep-id)
//	  -v checkout:/work -v gocache:/gocache -v gomodcache:/gomodcache
//	  -v lintcache:/lintcache -e K=V… --entrypoint '' <image> <argv…>
//
// --entrypoint with an EMPTY value precedes the image so an image whose ENTRYPOINT is git
// (docker.io/alpine/git) cannot swallow or reinterpret the gate command; the
// full argv is supplied explicitly after the image.
func (s ContainerSpec) BuildArgv(policy MountPolicy) ([]string, error) {
	bin := s.Runtime.Kind.Binary()
	switch {
	case bin == "":
		return nil, fmt.Errorf("%w: runtime kind %q", ErrContainerSpec, s.Runtime.Kind)
	case s.Image == "":
		return nil, fmt.Errorf("%w: image is empty", ErrContainerSpec)
	case s.Name == "":
		return nil, fmt.Errorf("%w: container name is empty", ErrContainerSpec)
	case len(s.Argv) == 0:
		return nil, fmt.Errorf("%w: argv is empty", ErrContainerSpec)
	case s.Checkout == "" || s.GoCache == "" || s.GoModCache == "" || s.LintCache == "":
		return nil, fmt.Errorf("%w: every mount source (checkout, gocache, gomodcache, lintcache) must be set", ErrContainerSpec)
	}
	endpoint, err := s.Runtime.EndpointArgs()
	if err != nil {
		return nil, err
	}
	if err := ForbidSocketMounts(policy, s.Checkout, s.GoCache, s.GoModCache, s.LintCache); err != nil {
		return nil, err
	}
	argv := append([]string{bin}, endpoint...)
	argv = append(argv, "run", "--rm", "--name", s.Name,
		"--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--workdir", MountWork,
	)
	if s.Runtime.Kind == KindPodman && s.Runtime.Rootless {
		argv = append(argv, "--userns=keep-id")
	} else {
		argv = append(argv, "--user", fmt.Sprintf("%d:%d", s.UID, s.GID))
	}
	argv = append(argv,
		"-v", s.Checkout+":"+MountWork,
		"-v", s.GoCache+":"+MountGoCache,
		"-v", s.GoModCache+":"+MountGoModCache,
		"-v", s.LintCache+":"+MountLintCache,
	)
	for _, kv := range s.Env {
		argv = append(argv, "-e", kv)
	}
	argv = append(argv, "--entrypoint", "", s.Image)
	argv = append(argv, s.Argv...)
	return argv, nil
}

// KillArgv is the command that removes the container after a timeout: killing
// the CLI does not stop the container, so the runner runs this on a detached
// context whenever the exec returns -1. It carries the same endpoint binding
// as BuildArgv, so cleanup reaches the daemon that ran the container and no
// other; it is nil when the runtime has no validated socket.
func (s ContainerSpec) KillArgv() []string {
	endpoint, err := s.Runtime.EndpointArgs()
	if err != nil {
		return nil
	}
	argv := append([]string{s.Runtime.Kind.Binary()}, endpoint...)
	return append(argv, "rm", "-f", s.Name)
}

// containerEnvPins are appended (drop-then-append) after the allow-list.
var containerEnvPins = []string{
	"HOME=/tmp",
	"GOPATH=/tmp/gopath",
	"GOCACHE=" + MountGoCache,
	"GOMODCACHE=" + MountGoModCache,
	"GOLANGCI_LINT_CACHE=" + MountLintCache,
	"GOPROXY=off",
	"GOTOOLCHAIN=local",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_SYSTEM=/dev/null",
}

// containerEnvAllowed reports whether a sanitized-env key crosses into the
// container: TZ/LANG/TERM, LC_*, CGO_*, GO*.
func containerEnvAllowed(key string) bool {
	switch key {
	case "TZ", "LANG", "TERM":
		return true
	}
	return strings.HasPrefix(key, "LC_") || strings.HasPrefix(key, "CGO_") || strings.HasPrefix(key, "GO")
}

// ContainerEnv projects the runner's sanitized gate env into the container:
// only the TZ/LANG/TERM/LC_*/CGO_*/GO* allow-list survives, then the cache
// and toolchain pins are appended drop-then-append (HOME, GOPATH, GOCACHE,
// GOMODCACHE, GOLANGCI_LINT_CACHE, GOPROXY=off, GOTOOLCHAIN=local,
// GIT_CONFIG_GLOBAL/SYSTEM=/dev/null), then extras drop-then-append so a
// caller-supplied value wins over both.
func ContainerEnv(sanitized []string, extras []string) []string {
	var out []string
	for _, kv := range sanitized {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !containerEnvAllowed(k) {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range containerEnvPins {
		out = dropThenAppend(out, kv)
	}
	for _, kv := range extras {
		if _, _, ok := strings.Cut(kv, "="); !ok {
			continue
		}
		out = dropThenAppend(out, kv)
	}
	return out
}

func dropThenAppend(env []string, kv string) []string {
	key, _, _ := strings.Cut(kv, "=")
	out := env[:0:0]
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == key {
			continue
		}
		out = append(out, e)
	}
	return append(out, kv)
}
