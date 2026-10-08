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
// WithCredentialDeny (E72.40 / #3792) extends the same profile past the
// network: it denies the macOS keychain's mach service, file reads of the
// runner host's credential paths under each named home (each path's
// symlink-resolved target too), and unix-socket connects under those homes,
// to the runner's ssh-agent socket and to the launchd Listeners pattern —
// the host credential surfaces a descendant could otherwise mint a forge
// credential from without any env var. A zero-option Profile render is
// byte-identical to the network-only profile.
//
// The package is a stdlib-only leaf mirroring gateiso/sandbox.go's
// probe/wrap shape. Long-form contract, the empirical matrix, and the
// stated residuals (nested sandbox_apply refused, unix sockets outside the
// credential denies open, Linux unavailable, a pre-existing hard link to a
// credential file, a symlink re-pointed after the render): README.md next
// to this file.
package netsandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os/exec"
	"path/filepath"
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
//
// opts add optional sections (WithCredentialDeny), rendered between
// `(allow default)` and the IP deny; with no opts the render is
// byte-identical to the network-only profile. An option's error fails the
// whole render the same way.
func Profile(proxyAddr string, allowHosts []string, opts ...ProfileOption) (string, error) {
	var o profileOptions
	for _, opt := range opts {
		opt(&o)
	}
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
	var cred string
	if o.cred != nil {
		if cred, err = credentialClauses(*o.cred); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	// The credential section renders BEFORE the IP rules, never after the
	// port allows: a `(deny network-outbound (remote unix-socket ...))`
	// rendered after `(allow network-outbound (remote ip "localhost:<p>"))`
	// also denies the TCP connect to <p> for a port-dependent subset of
	// ports (~15% of ephemeral ports on Darwin 25.6 — the proxy path breaks
	// at random). Rendered first, the IP allow is the last match for every
	// IP connect and the unix-socket denies still bind (#3792).
	b.WriteString(cred)
	b.WriteString(denyClause)
	b.WriteString("\n")
	for _, p := range sorted {
		fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", p)
	}
	return b.String(), nil
}

// ProfileOption adds an optional section to a Profile render.
type ProfileOption func(*profileOptions)

type profileOptions struct {
	cred *CredentialDeny
}

// KeychainMachService is the mach service every keychain read goes
// through (Security.framework → securityd). Denying its lookup refuses
// `security find-generic-password` and `gh auth token`'s keyring read
// regardless of which keychain FILE is named, while TLS verification (the
// system trust store) keeps working.
const KeychainMachService = "com.apple.SecurityServer"

// LaunchdListenersPattern matches the launchd-vended per-user socket
// directory macOS hands to SSH_AUTH_SOCK by default
// (/private/tmp/com.apple.launchd.<random>/Listeners), so a descendant that
// guesses the system ssh-agent socket without the runner's env is refused.
const LaunchdListenersPattern = `^/private/tmp/com\.apple\.launchd\.[^/]+/Listeners$`

// CredentialDeny names the runner host's credential surfaces a Profile
// render denies (E72.40 / #3792). Homes and AgentSockets must already be
// canonical (absolute, filepath.Clean-equal, symlink-resolved by the
// caller); a path that is not, or that carries a `"`, `\` or control
// character, fails the render.
type CredentialDeny struct {
	// Homes are the real home directories whose credential paths and
	// unix sockets are denied.
	Homes []string
	// Files denies reads of each home's credential files and directories
	// (.ssh, .config/gh, .config/glab-cli, .gitconfig, .git-credentials,
	// .config/git/credentials, .netrc) and of each one's symlink-resolved
	// target when it differs.
	Files bool
	// Keychain denies the KeychainMachService lookup and reads of each
	// home's Library/Keychains (and its resolved target).
	Keychain bool
	// SSHAgent denies unix-socket connects under each home, to each
	// AgentSockets path, and to LaunchdListenersPattern.
	SSHAgent bool
	// AgentSockets are the runner's resolved SSH_AUTH_SOCK path(s).
	AgentSockets []string
	// EvalSymlinks resolves each credential path; nil means
	// filepath.EvalSymlinks. A not-exist error denies the unresolved path
	// only; any other error fails the render (fail closed).
	EvalSymlinks func(string) (string, error)
}

// WithCredentialDeny appends d's credential-surface denies to the profile.
func WithCredentialDeny(d CredentialDeny) ProfileOption {
	return func(o *profileOptions) { o.cred = &d }
}

// credEntry is one credential path relative to a home. A dir entry renders
// as a Seatbelt `subpath` (the path and everything under it, including a
// directory created later), a file entry as a `literal`.
type credEntry struct {
	rel string
	dir bool
}

var (
	keychainEntries = []credEntry{{"Library/Keychains", true}}
	fileEntries     = []credEntry{
		{".ssh", true},
		{".config/gh", true},
		{".config/glab-cli", true},
		{".gitconfig", false},
		{".git-credentials", false},
		{".config/git/credentials", false},
		{".netrc", false},
	}
)

// credentialClauses renders d's deny clauses in a fixed order — keychain,
// files, unix sockets — with homes, filters and sockets de-duplicated and
// sorted, so the output is byte-deterministic across input orderings.
// Profile places the result ahead of the IP rules (see the comment there).
func credentialClauses(d CredentialDeny) (string, error) {
	eval := d.EvalSymlinks
	if eval == nil {
		eval = filepath.EvalSymlinks
	}
	homes, err := canonicalSet("home", d.Homes)
	if err != nil {
		return "", err
	}
	sockets, err := canonicalSet("agent socket", d.AgentSockets)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if d.Keychain {
		b.WriteString(`(deny mach-lookup (global-name "` + KeychainMachService + `"))` + "\n")
		for _, h := range homes {
			if err := writeFileReadDeny(&b, h, keychainEntries, eval); err != nil {
				return "", err
			}
		}
	}
	if d.Files {
		for _, h := range homes {
			if err := writeFileReadDeny(&b, h, fileEntries, eval); err != nil {
				return "", err
			}
		}
	}
	if d.SSHAgent {
		for _, h := range homes {
			b.WriteString(`(deny network-outbound (remote unix-socket (subpath "` + h + `")))` + "\n")
		}
		for _, s := range sockets {
			b.WriteString(`(deny network-outbound (remote unix-socket (path-literal "` + s + `")))` + "\n")
		}
		b.WriteString(`(deny network-outbound (remote unix-socket (path-regex #"` + LaunchdListenersPattern + `")))` + "\n")
	}
	return b.String(), nil
}

// writeFileReadDeny renders one `(deny file-read* ...)` clause for home
// holding, per entry, the unresolved path and — when it resolves somewhere
// else — the resolved target. Seatbelt checks each vnode a lookup meets
// against that vnode's OWN path, so the unresolved filter alone leaves the
// target readable directly or through any other alias, and an
// intermediate-component symlink (~/.config → elsewhere) bypasses it
// entirely; the resolved filter closes both.
func writeFileReadDeny(b *strings.Builder, home string, entries []credEntry, eval func(string) (string, error)) error {
	set := map[string]struct{}{}
	for _, e := range entries {
		p := filepath.Join(home, e.rel)
		paths := []string{p}
		r, err := eval(p)
		switch {
		case err == nil:
			if r != p {
				if err := validatePath("resolved target of "+p, r); err != nil {
					return err
				}
				paths = append(paths, r)
			}
		case errors.Is(err, fs.ErrNotExist):
			// Nothing there yet: deny the path itself so a later
			// creation is still covered.
		default:
			return fmt.Errorf("netsandbox: resolving credential path %q: %w", p, err)
		}
		kind := "literal"
		if e.dir {
			kind = "subpath"
		}
		for _, q := range paths {
			set["("+kind+` "`+q+`")`] = struct{}{}
		}
	}
	filters := make([]string, 0, len(set))
	for f := range set {
		filters = append(filters, f)
	}
	sort.Strings(filters)
	b.WriteString("(deny file-read*")
	for _, f := range filters {
		b.WriteString("\n  " + f)
	}
	b.WriteString(")\n")
	return nil
}

// canonicalSet validates every path, then returns them de-duplicated and
// sorted.
func canonicalSet(what string, in []string) ([]string, error) {
	set := map[string]struct{}{}
	for _, p := range in {
		if err := validatePath(what, p); err != nil {
			return nil, err
		}
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// validatePath refuses a path that cannot be rendered into a Seatbelt
// string literal verbatim or that is not canonical: not absolute, not
// filepath.Clean-equal, or carrying a `"`, `\` or control character.
func validatePath(what, p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("netsandbox: %s %q is not absolute", what, p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("netsandbox: %s %q is not a clean path", what, p)
	}
	for _, r := range p {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("netsandbox: %s %q contains a quote, backslash or control character", what, p)
		}
	}
	return nil
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
