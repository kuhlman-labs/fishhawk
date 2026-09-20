// Package netsandbox confines the acceptance agent's WHOLE process tree at
// the OS layer so no descendant can opt out of the ADR-050 egress control by
// clearing its environment (#3393).
//
// The env-carried controls (HTTP(S)_PROXY pointed at the egress proxy,
// NO_PROXY cleared, FISHHAWK_FORGE_WRITES=deny, forge credentials withheld)
// bind only cooperating processes: a nested `env -i fishhawk-runner …`
// inherits none of them, mints its own forge credential through non-env
// fallbacks, and pushes to the real repository — the incident this package
// closes. A Seatbelt profile applied by sandbox-exec(1) is inherited by
// every descendant regardless of its env, so a direct connect() to anything
// but the admitted loopback ports fails with EPERM at the kernel: `git push`
// (HTTPS or SSH), a Go binary dialing the forge, a curl with no proxy vars,
// all refused. Public hostnames are NEVER admitted direct — they are
// reachable only through the proxy, which is exactly today's sanctioned
// path.
//
// The package is a stdlib-only leaf mirroring gateiso/sandbox.go's
// probe/wrap shape. Long-form contract, the empirical matrix, and the
// stated residuals (nested sandbox_apply refused, unix sockets open, Linux
// unavailable): README.md next to this file.
package netsandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Mode is the FISHHAWK_ACCEPTANCE_NET_SANDBOX policy.
type Mode string

const (
	// ModeAuto applies the sandbox when the host can provide it and
	// otherwise proceeds with a LOUD unavailable event — the default.
	ModeAuto Mode = "auto"
	// ModeRequire fails the stage pre-spawn when the sandbox is unavailable.
	ModeRequire Mode = "require"
	// ModeOff never applies the sandbox (the kill switch for an agent
	// breakage under the profile, e.g. a nested built-in sandbox).
	ModeOff Mode = "off"
)

// ParseMode maps the operator string to a Mode. Empty is ModeAuto; anything
// else must be one of the three values exactly (case-sensitive, trimmed),
// and the error names the valid values.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(s)); m {
	case "":
		return ModeAuto, nil
	case ModeAuto, ModeRequire, ModeOff:
		return m, nil
	default:
		return "", fmt.Errorf("netsandbox: invalid mode %q: valid values are %q, %q, %q", s, ModeAuto, ModeRequire, ModeOff)
	}
}

// UnavailableNonDarwin is the reason Probe reports on every non-darwin host.
// It names the Linux gap honestly: gateiso's `unshare -n` mechanism creates a
// namespace with NO route to the host loopback the egress proxy binds, so it
// cannot express "host loopback reachable, everything else denied" — the
// acceptance case is the opposite of the verify gate's fresh-namespace need.
// Enforcement there stays env-only; a Linux equivalent is a follow-up.
const UnavailableNonDarwin = "net-sandbox unavailable: Seatbelt (sandbox-exec) is macOS-only; unshare -n cannot see the host loopback the egress proxy binds, so no Linux equivalent ships yet — enforcement is env-only on this OS"

// Probe reports whether the Seatbelt network sandbox is usable on this
// host: darwin, sandbox-exec on PATH, and a deny-all profile actually
// applies against a no-op (a future macOS may ship the binary but refuse
// the profile, so presence is not enough). The reason names what is
// missing when unavailable.
func Probe(ctx context.Context) (available bool, reason string) {
	return probe(ctx, runtime.GOOS, exec.LookPath, probeExec)
}

// probeExec runs the probe command and folds its combined output into the
// returned error so an unavailable reason names what sandbox-exec printed.
func probeExec(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// probeProfile is the deny-all profile the probe applies against /usr/bin/true.
const probeProfile = "(version 1)\n(allow default)\n" + denyClause + "\n"

// probe is Probe with its host probes injected.
func probe(ctx context.Context, goos string, lookPath func(string) (string, error), run func(context.Context, string, ...string) error) (bool, string) {
	if goos != "darwin" {
		return false, UnavailableNonDarwin
	}
	if _, err := lookPath(sandboxExec); err != nil {
		return false, fmt.Sprintf("net-sandbox unavailable: %s not on PATH (%v)", sandboxExec, err)
	}
	argv := Wrap([]string{"/usr/bin/true"}, probeProfile)
	if err := run(ctx, argv[0], argv[1:]...); err != nil {
		return false, fmt.Sprintf("net-sandbox unavailable: sandbox-exec deny-all probe failed (%v)", err)
	}
	return true, ""
}

const (
	sandboxExec = "sandbox-exec"
	// denyClause refuses every outbound IP connection; the per-port allows
	// rendered after it are the only exceptions. Unix-domain sockets are
	// NOT covered by `remote ip` and stay open (stated residual).
	denyClause = `(deny network-outbound (remote ip "*:*"))`
)

// Profile renders the deterministic Seatbelt profile for one acceptance
// invocation: deny every outbound IP connection, then allow exactly the
// admitted loopback endpoints — proxyAddr's port always, plus each
// allowHosts entry whose host is `localhost` or a loopback IP literal
// (127.0.0.0/8, ::1). A host-only loopback entry expands to ports 80 and
// 443, matching egressproxy's host-only semantics. Every NON-loopback
// entry (api.anthropic.com, the forge host, a LAN IP) is silently NOT
// admitted direct: it is reachable only through the proxy.
//
// Seatbelt's `remote ip` grammar accepts only `localhost` or `*` as the
// host (an IP literal is a profile syntax error), and `localhost:<port>`
// matches BOTH 127.0.0.1 and ::1 (verified on Darwin 25.6), so every
// admitted endpoint renders as `localhost:<port>`. Output is byte-
// deterministic across input orderings: ports are de-duplicated and
// sorted numerically.
//
// proxyAddr must be host:port with a loopback host; a non-loopback proxy,
// or a non-numeric / out-of-range port anywhere, returns an error — fail
// closed rather than render an over-broad or malformed profile.
func Profile(proxyAddr string, allowHosts []string) (string, error) {
	host, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return "", fmt.Errorf("netsandbox: proxy address %q: %w", proxyAddr, err)
	}
	if !isLoopbackHost(host) {
		return "", fmt.Errorf("netsandbox: proxy address %q is not loopback; refusing to render a profile that admits a non-loopback endpoint direct", proxyAddr)
	}
	p, err := parsePort(port)
	if err != nil {
		return "", fmt.Errorf("netsandbox: proxy address %q: %w", proxyAddr, err)
	}
	ports := map[int]struct{}{p: {}}
	for _, raw := range allowHosts {
		s := strings.ToLower(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		h, pt, err := net.SplitHostPort(s)
		if err != nil {
			// Host-only entry (no port). A bracketed IPv6 literal without a
			// port also lands here; strip the brackets before classifying.
			h, pt = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), ""
		}
		if !isLoopbackHost(h) {
			continue
		}
		if pt == "" {
			ports[80], ports[443] = struct{}{}, struct{}{}
			continue
		}
		p, err := parsePort(pt)
		if err != nil {
			return "", fmt.Errorf("netsandbox: allow-list entry %q: %w", raw, err)
		}
		ports[p] = struct{}{}
	}
	sorted := make([]int, 0, len(ports))
	for p := range ports {
		sorted = append(sorted, p)
	}
	sort.Ints(sorted)
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	b.WriteString(denyClause)
	b.WriteString("\n")
	for _, p := range sorted {
		fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", p)
	}
	return b.String(), nil
}

// AdmittedPorts returns the loopback ports a Profile-rendered profile admits,
// in the rendered order, for event logging. It parses the profile rather
// than recomputing, so the logged list is what the kernel enforces.
func AdmittedPorts(profile string) []int {
	var out []int
	for _, line := range strings.Split(profile, "\n") {
		const prefix = `(allow network-outbound (remote ip "localhost:`
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		if i := strings.Index(rest, `"`); i > 0 {
			if p, err := strconv.Atoi(rest[:i]); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// Wrap returns argv wrapped in sandbox-exec so it executes under profile:
// ["sandbox-exec", "-p", profile, argv...]. argv is appended verbatim,
// never interpolated into the profile, so a command carrying quotes or
// shell metacharacters is executed as-is.
func Wrap(argv []string, profile string) []string {
	out := make([]string, 0, 3+len(argv))
	out = append(out, sandboxExec, "-p", profile)
	return append(out, argv...)
}

// isLoopbackHost reports whether host is `localhost` or a loopback IP
// literal (127.0.0.0/8 or ::1).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parsePort validates a numeric TCP port in 1..65535.
func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("port %q is not numeric", s)
	}
	if p < 1 || p > 65535 {
		return 0, errors.New("port " + s + " is out of range 1..65535")
	}
	return p, nil
}
