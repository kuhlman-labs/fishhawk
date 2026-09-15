package gateiso

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// sandboxScript is the shell body WrapSandbox runs inside the fresh network
// namespace: bring loopback up (unshare -n creates it DOWN) and exec the
// wrapped argv verbatim through "$0" "$@" — argv is passed as positional
// parameters, never interpolated into the script, so a gate command
// carrying quotes or shell metacharacters is executed as-is.
const sandboxScript = `ip link set lo up && exec "$0" "$@"`

// SandboxUnavailableNonLinux is the reason ProbeSandbox reports on every
// non-Linux host. It names the ADR-063 gap: macOS has no unprivileged
// no-network sandbox primitive, so the clone-sandbox path is Linux-only and
// a macOS runner falls back to the plain clone path (or the container path
// when FISHHAWK_GATE_IMAGE is configured).
const SandboxUnavailableNonLinux = "clone-sandbox unavailable: unshare(1) network namespaces are Linux-only (ADR-063 gap: no unprivileged no-network sandbox on this OS; configure FISHHAWK_GATE_IMAGE for the container path or accept the clone path)"

// ProbeSandbox reports whether the Linux no-network sandbox is usable on
// this host: unshare(1) and ip(8) are on PATH and an unprivileged
// user+network namespace can actually be created (distributions and
// seccomp profiles may disable unprivileged user namespaces, so presence
// of the binaries is not enough — the probe runs the real wrapper once
// against a no-op). The reason names what is missing when unavailable.
func ProbeSandbox(ctx context.Context) (available bool, reason string) {
	return probeSandbox(ctx, runtime.GOOS, exec.LookPath, sandboxProbeExec)
}

// sandboxProbeExec runs the probe command and folds its combined output into
// the returned error so an unavailable reason names what unshare printed.
func sandboxProbeExec(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// probeSandbox is ProbeSandbox with its host probes injected.
func probeSandbox(ctx context.Context, goos string, lookPath func(string) (string, error), run func(context.Context, string, ...string) error) (bool, string) {
	if goos != "linux" {
		return false, SandboxUnavailableNonLinux
	}
	for _, bin := range []string{"unshare", "ip"} {
		if _, err := lookPath(bin); err != nil {
			return false, fmt.Sprintf("clone-sandbox unavailable: %s not on PATH (%v)", bin, err)
		}
	}
	probe := WrapSandbox([]string{"true"})
	if err := run(ctx, probe[0], probe[1:]...); err != nil {
		return false, fmt.Sprintf("clone-sandbox unavailable: unprivileged user+network namespace probe failed (%v)", err)
	}
	return true, ""
}

// WrapSandbox returns argv wrapped so it executes inside a fresh network
// namespace (unshare -n) with loopback up, mapped to root in a new user
// namespace (unshare -r) so `ip link set lo up` is permitted without
// privilege. The namespace has NO interface but lo, so the wrapped command
// cannot reach the host loopback, the LAN or the internet. argv is passed
// through "$0" "$@" — never interpolated into the shell script.
func WrapSandbox(argv []string) []string {
	out := make([]string, 0, 6+len(argv))
	out = append(out, "unshare", "-rn", "--", "sh", "-c", sandboxScript)
	return append(out, argv...)
}
