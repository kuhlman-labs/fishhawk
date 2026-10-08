package netsandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/egressproxy"
)

const header = "(version 1)\n(allow default)\n(deny network-outbound (remote ip \"*:*\"))\n"

func allow(port string) string {
	return "(allow network-outbound (remote ip \"localhost:" + port + "\"))\n"
}

func TestParseMode_Table(t *testing.T) {
	rows := []struct {
		in   string
		want Mode
		err  bool
	}{
		{"", ModeAuto, false},
		{"auto", ModeAuto, false},
		{" require ", ModeRequire, false},
		{"off", ModeOff, false},
		{"AUTO", "", true},
		{"strict", "", true},
	}
	for _, r := range rows {
		got, err := ParseMode(r.in)
		if (err != nil) != r.err || got != r.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q, err=%v", r.in, got, err, r.want, r.err)
		}
		if err != nil {
			for _, v := range []string{`"auto"`, `"require"`, `"off"`} {
				if !strings.Contains(err.Error(), v) {
					t.Errorf("ParseMode(%q) error %q does not name %s", r.in, err, v)
				}
			}
		}
	}
}

func TestProfile_Golden(t *testing.T) {
	rows := []struct {
		name  string
		proxy string
		hosts []string
		want  string
		err   string
	}{
		{"proxy port always admitted, nothing else", "127.0.0.1:8090", nil, header + allow("8090"), ""},
		{"host-only loopback expands to 80+443", "127.0.0.1:8090", []string{"localhost"}, header + allow("80") + allow("443") + allow("8090"), ""},
		{"localhost:port admitted", "127.0.0.1:8090", []string{"localhost:3000"}, header + allow("3000") + allow("8090"), ""},
		{"127.0.0.1:port admitted", "127.0.0.1:8090", []string{"127.0.0.1:3000"}, header + allow("3000") + allow("8090"), ""},
		{"[::1]:port admitted", "127.0.0.1:8090", []string{"[::1]:3000"}, header + allow("3000") + allow("8090"), ""},
		{"127.0.0.0/8 literal admitted", "127.0.0.1:8090", []string{"127.0.0.2:3000"}, header + allow("3000") + allow("8090"), ""},
		{"public hostnames NOT admitted direct", "127.0.0.1:8090", []string{"api.anthropic.com", "api.github.com", "github.com:443"}, header + allow("8090"), ""},
		{"LAN IP NOT admitted direct", "127.0.0.1:8090", []string{"10.0.0.5:443", "192.168.1.2"}, header + allow("8090"), ""},
		{"duplicates collapse", "127.0.0.1:8090", []string{"localhost:3000", "127.0.0.1:3000", "[::1]:3000", "localhost:8090"}, header + allow("3000") + allow("8090"), ""},
		{"host-only ::1 expands", "127.0.0.1:8090", []string{"::1"}, header + allow("80") + allow("443") + allow("8090"), ""},
		{"empty entries ignored", "127.0.0.1:8090", []string{"", "  "}, header + allow("8090"), ""},
		{"ipv6 proxy", "[::1]:8090", nil, header + allow("8090"), ""},
		{"bad port letters", "127.0.0.1:8090", []string{"localhost:abc"}, "", `"localhost:abc": port "abc" is not numeric`},
		{"bad port range", "127.0.0.1:8090", []string{"localhost:70000"}, "", `"localhost:70000": port 70000 is out of range`},
		{"bad port zero", "127.0.0.1:8090", []string{"localhost:0"}, "", "out of range"},
		{"non-loopback proxy refused", "10.0.0.5:8090", nil, "", "is not loopback"},
		{"proxy without port refused", "127.0.0.1", nil, "", "missing port"},
		{"proxy bad port refused", "127.0.0.1:x", nil, "", "not numeric"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got, err := Profile(r.proxy, r.hosts)
			if r.err != "" {
				if err == nil || !strings.Contains(err.Error(), r.err) {
					t.Fatalf("err = %v, want containing %q", err, r.err)
				}
				if got != "" {
					t.Fatalf("profile must be empty on error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != r.want {
				t.Fatalf("profile =\n%s\nwant\n%s", got, r.want)
			}
		})
	}
}

func TestProfile_DeterministicAcrossOrderings(t *testing.T) {
	a, err := Profile("127.0.0.1:8090", []string{"localhost:9000", "[::1]:80", "127.0.0.1:443", "api.github.com"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Profile("127.0.0.1:8090", []string{"api.github.com", "127.0.0.1:443", "[::1]:80", "localhost:9000"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("orderings differ:\n%s\nvs\n%s", a, b)
	}
	if want := header + allow("80") + allow("443") + allow("8090") + allow("9000"); a != want {
		t.Fatalf("profile =\n%s\nwant\n%s", a, want)
	}
	if got := AdmittedPorts(a); !reflect.DeepEqual(got, []int{80, 443, 8090, 9000}) {
		t.Fatalf("AdmittedPorts = %v", got)
	}
}

func TestProbe_Rows(t *testing.T) {
	okLook := func(string) (string, error) { return "/usr/bin/sandbox-exec", nil }
	okRun := func(context.Context, string, ...string) error { return nil }
	rows := []struct {
		name      string
		goos      string
		lookPath  func(string) (string, error)
		run       func(context.Context, string, ...string) error
		available bool
		reason    string
	}{
		{"linux is the stated gap", "linux", okLook, okRun, false, UnavailableNonDarwin},
		{"windows likewise", "windows", okLook, okRun, false, UnavailableNonDarwin},
		{"darwin without sandbox-exec", "darwin", func(string) (string, error) {
			return "", errors.New("not found")
		}, okRun, false, "sandbox-exec not on PATH"},
		{"darwin probe exec fails", "darwin", okLook, func(context.Context, string, ...string) error {
			return errors.New("exit status 71: sandbox_apply: Operation not permitted")
		}, false, "deny-all probe failed (exit status 71: sandbox_apply: Operation not permitted)"},
		{"darwin with a working probe", "darwin", okLook, okRun, true, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var seen []string
			run := func(ctx context.Context, name string, args ...string) error {
				seen = append([]string{name}, args...)
				return row.run(ctx, name, args...)
			}
			got, reason := probe(context.Background(), row.goos, row.lookPath, run)
			if got != row.available {
				t.Fatalf("available = %v, want %v (reason %q)", got, row.available, reason)
			}
			if !strings.Contains(reason, row.reason) {
				t.Fatalf("reason %q does not contain %q", reason, row.reason)
			}
			if row.goos == "darwin" && row.available {
				// The probe runs the REAL wrapper against a no-op under a
				// profile carrying the deny clause, not a bespoke command.
				want := Wrap([]string{"/usr/bin/true"}, probeProfile)
				if !reflect.DeepEqual(seen, want) {
					t.Fatalf("probe ran %q, want the Wrap form %q", seen, want)
				}
				if !strings.Contains(seen[2], denyClause) {
					t.Fatalf("probe profile lacks the deny clause: %q", seen[2])
				}
			}
			if row.goos != "darwin" && seen != nil {
				t.Fatalf("non-darwin probe must not exec anything, ran %q", seen)
			}
		})
	}
}

func TestWrap_PositionalPassthrough(t *testing.T) {
	argv := []string{"/usr/local/bin/claude", "-p", `it's "quoted" $(rm -rf /) with spaces`, "--model", "x"}
	profile := "(version 1)\n(allow default)\n"
	got := Wrap(argv, profile)
	want := append([]string{"sandbox-exec", "-p", profile}, argv...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Wrap =\n%q\nwant\n%q", got, want)
	}
	if got[2] != profile {
		t.Fatalf("profile must be passed verbatim, got %q", got[2])
	}
}

// TestSeatbelt_DeniesDirectEgress_ProxyPathSurvives is the darwin-only
// end-to-end: a REAL sandbox-exec, a REAL egressproxy, and httptest
// forge/preview servers. It is what proves the mechanism closes the
// #3393 escape (a descendant with a CLEARED env cannot reach the forge)
// without breaking the sanctioned proxy path.
//
// Cases (a)–(c) and the IPv6 case are hermetic (loopback only). Case (d) is
// NON-HERMETIC: it dials a public IP literal and proves the refusal is an
// EPERM at connect() (curl exit 7 in well under the -m 3 timeout), which a
// host without egress cannot distinguish from a network timeout — that is
// why its assertion is the FAST exit, not merely a non-zero one.
func TestSeatbelt_DeniesDirectEgress_ProxyPathSurvives(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt e2e is darwin-only (the Linux gap is the stated residual)")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not on PATH")
	}
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("/usr/bin/curl absent")
	}
	if avail, reason := Probe(context.Background()); !avail {
		t.Skipf("net sandbox unavailable on this host: %s", reason)
	}

	var forgeHits atomic.Int32
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forgeHits.Add(1)
		w.WriteHeader(200)
	}))
	defer forge.Close()
	preview := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer preview.Close()
	// IPv6 loopback server admitted via the SAME localhost:<port> spelling.
	ln6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	v6 := &httptest.Server{Listener: ln6, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}), ReadHeaderTimeout: time.Second}}
	v6.Start()
	defer v6.Close()

	forgeHost := strings.TrimPrefix(forge.URL, "http://")
	previewHost := strings.TrimPrefix(preview.URL, "http://")
	_, v6Port, _ := net.SplitHostPort(ln6.Addr().String())

	// The proxy admits the forge (as the runner's proxy admits the real
	// forge host); the Seatbelt profile does NOT — it admits only the proxy
	// port and the preview/v6 loopback ports.
	proxy, err := egressproxy.Start(egressproxy.Config{AllowHosts: []string{forgeHost}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	proxyAddr := strings.TrimPrefix(proxy.URL(), "http://")
	profile, err := Profile(proxyAddr, []string{previewHost, "localhost:" + v6Port})
	if err != nil {
		t.Fatal(err)
	}

	curl := func(t *testing.T, env []string, url string, timeoutSecs string) (code int, body string, elapsed time.Duration) {
		t.Helper()
		argv := Wrap([]string{"/usr/bin/curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "-m", timeoutSecs, url}, profile)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = env
		start := time.Now()
		out, err := cmd.CombinedOutput()
		elapsed = time.Since(start)
		code = 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("curl spawn: %v", err)
		}
		return code, string(out), elapsed
	}

	t.Run("a_preview_direct_allowed", func(t *testing.T) {
		code, body, _ := curl(t, os.Environ(), preview.URL+"/", "5")
		if code != 0 || body != "200" {
			t.Fatalf("preview direct: exit %d body %q, want 0/200", code, body)
		}
	})
	t.Run("b_forge_direct_cleared_env_denied", func(t *testing.T) {
		// The incident's exact shape: a descendant with NO env at all.
		argv := Wrap([]string{"/usr/bin/env", "-i", "/usr/bin/curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "-m", "5", forge.URL + "/"}, profile)
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 7 {
			t.Fatalf("forge direct under env -i: err %v out %q, want curl exit 7 (connect refused by EPERM)", err, out)
		}
		if n := forgeHits.Load(); n != 0 {
			t.Fatalf("forge recorded %d hits, want 0 — the sandbox did not confine the cleared-env descendant", n)
		}
	})
	t.Run("c_forge_via_proxy_allowed", func(t *testing.T) {
		code, body, _ := curl(t, []string{"http_proxy=" + proxy.URL(), "PATH=/usr/bin"}, forge.URL+"/", "5")
		if code != 0 || body != "200" {
			t.Fatalf("forge via proxy: exit %d body %q, want 0/200 (the sanctioned path must survive)", code, body)
		}
		if n := forgeHits.Load(); n != 1 {
			t.Fatalf("forge hits = %d, want exactly 1 (through the proxy)", n)
		}
	})
	t.Run("d_public_ip_literal_eperm_fast_NON_HERMETIC", func(t *testing.T) {
		// NON-HERMETIC: touches a public IP literal (a GitHub front). The
		// assertion is the FAST exit-7 — EPERM at connect(), not a -m 3
		// network timeout — so a host without egress cannot green this by
		// timing out.
		code, _, elapsed := curl(t, os.Environ(), "https://140.82.112.3/", "3")
		if code != 7 {
			t.Fatalf("public IP literal: exit %d, want 7 (EPERM)", code)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("public IP literal took %s — a timeout, not an EPERM refusal", elapsed)
		}
	})
	t.Run("ipv6_loopback_admitted_via_localhost", func(t *testing.T) {
		code, body, _ := curl(t, os.Environ(), "http://[::1]:"+v6Port+"/", "5")
		if code != 0 || body != "200" {
			t.Fatalf("[::1] via localhost:<port>: exit %d body %q, want 0/200", code, body)
		}
	})
	t.Run("loopback_port_not_admitted_denied", func(t *testing.T) {
		// A loopback server whose port the profile does not admit (the
		// forge) is denied direct even from a full env — the deny is
		// per-port, not per-host.
		code, _, _ := curl(t, os.Environ(), forge.URL+"/", "5")
		if code != 7 {
			t.Fatalf("forge direct with full env: exit %d, want 7", code)
		}
		if n := forgeHits.Load(); n != 1 {
			t.Fatalf("forge hits = %d, want still 1", n)
		}
	})
}

// fakeResolver is an injected EvalSymlinks over a fixed map: a mapped path
// resolves to its value, every other path is fs.ErrNotExist. It keeps the
// credential-deny render tests filesystem-free.
func fakeResolver(m map[string]string) func(string) (string, error) {
	return func(p string) (string, error) {
		if r, ok := m[p]; ok {
			return r, nil
		}
		return "", fmt.Errorf("lstat %s: %w", p, fs.ErrNotExist)
	}
}

// credGoldenResolver resolves three /Users/a credential paths elsewhere
// (/x/...), one /home/b path to itself (exists, not a symlink), and leaves
// everything else not-exist.
var credGoldenResolver = map[string]string{
	"/Users/a/.ssh":             "/x/ssh",
	"/Users/a/.git-credentials": "/x/creds",
	"/Users/a/.config/gh":       "/x/cfg/gh",
	"/home/b/.netrc":            "/home/b/.netrc",
}

const credGoldenSection = `(deny mach-lookup (global-name "com.apple.SecurityServer"))
(deny file-read*
  (subpath "/Users/a/Library/Keychains"))
(deny file-read*
  (subpath "/home/b/Library/Keychains"))
(deny file-read*
  (literal "/Users/a/.config/git/credentials")
  (literal "/Users/a/.git-credentials")
  (literal "/Users/a/.gitconfig")
  (literal "/Users/a/.netrc")
  (literal "/x/creds")
  (subpath "/Users/a/.config/gh")
  (subpath "/Users/a/.config/glab-cli")
  (subpath "/Users/a/.ssh")
  (subpath "/x/cfg/gh")
  (subpath "/x/ssh"))
(deny file-read*
  (literal "/home/b/.config/git/credentials")
  (literal "/home/b/.git-credentials")
  (literal "/home/b/.gitconfig")
  (literal "/home/b/.netrc")
  (subpath "/home/b/.config/gh")
  (subpath "/home/b/.config/glab-cli")
  (subpath "/home/b/.ssh"))
(deny network-outbound (remote unix-socket (subpath "/Users/a")))
(deny network-outbound (remote unix-socket (subpath "/home/b")))
(deny network-outbound (remote unix-socket (path-literal "/private/tmp/agent.sock")))
(deny network-outbound (remote unix-socket (path-literal "/var/run/b.sock")))
(deny network-outbound (remote unix-socket (path-regex #"^/private/tmp/com\.apple\.launchd\.[^/]+/Listeners$")))
`

// permutations returns every ordering of in.
func permutations(in []string) [][]string {
	if len(in) <= 1 {
		return [][]string{append([]string(nil), in...)}
	}
	var out [][]string
	for i := range in {
		rest := append(append([]string(nil), in[:i]...), in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{in[i]}, p...))
		}
	}
	return out
}

// TestProfile_CredentialDeny_Golden (#3792, control 7) pins the byte-exact
// credential render between `(allow default)` and the unchanged IP rules
// (the unix-socket denies must precede the port allows — see Profile): the
// resolved
// subpath/literal filters beside the unresolved ones, only the unresolved
// filter for a not-exist path, a self-resolving path rendered once, and
// identical output for every ordering of two homes and duplicate sockets.
func TestProfile_CredentialDeny_Golden(t *testing.T) {
	want := "(version 1)\n(allow default)\n" + credGoldenSection + strings.TrimPrefix(header, "(version 1)\n(allow default)\n") + allow("8090")
	for _, homes := range permutations([]string{"/Users/a", "/home/b"}) {
		for _, socks := range permutations([]string{"/private/tmp/agent.sock", "/var/run/b.sock", "/private/tmp/agent.sock"}) {
			got, err := Profile("127.0.0.1:8090", nil, WithCredentialDeny(CredentialDeny{
				Homes: homes, Files: true, Keychain: true, SSHAgent: true, AgentSockets: socks,
				EvalSymlinks: fakeResolver(credGoldenResolver),
			}))
			if err != nil {
				t.Fatalf("homes %v socks %v: %v", homes, socks, err)
			}
			if got != want {
				t.Fatalf("homes %v socks %v: profile =\n%s\nwant\n%s", homes, socks, got, want)
			}
		}
	}
}

// TestProfile_CredentialDeny_FlagsGateSections pins that each flag renders
// only its own section: auto-without-credential (Files + SSHAgent, no
// Keychain) carries no mach-lookup and no Keychains clause, and a zero
// CredentialDeny renders the network-only profile byte-for-byte.
func TestProfile_CredentialDeny_FlagsGateSections(t *testing.T) {
	base, err := Profile("127.0.0.1:8090", nil)
	if err != nil {
		t.Fatal(err)
	}
	render := func(d CredentialDeny) string {
		t.Helper()
		d.Homes = []string{"/Users/a"}
		d.EvalSymlinks = fakeResolver(nil)
		got, err := Profile("127.0.0.1:8090", nil, WithCredentialDeny(d))
		if err != nil {
			t.Fatal(err)
		}
		const pre = "(version 1)\n(allow default)\n"
		section, ok := strings.CutPrefix(got, pre)
		if !ok || !strings.HasSuffix(section, strings.TrimPrefix(base, pre)) {
			t.Fatalf("render does not wrap the credential section around the unchanged network rules:\n%s", got)
		}
		return strings.TrimSuffix(section, strings.TrimPrefix(base, pre))
	}
	if got := render(CredentialDeny{}); got != "" {
		t.Errorf("zero CredentialDeny appended %q, want nothing", got)
	}
	rows := []struct {
		name     string
		d        CredentialDeny
		has, not []string
	}{
		{"keychain only", CredentialDeny{Keychain: true}, []string{"mach-lookup", "/Users/a/Library/Keychains"}, []string{".ssh", "unix-socket"}},
		{"files only", CredentialDeny{Files: true}, []string{`(subpath "/Users/a/.ssh")`, `(literal "/Users/a/.gitconfig")`}, []string{"mach-lookup", "Keychains", "unix-socket"}},
		{"ssh agent only", CredentialDeny{SSHAgent: true, AgentSockets: []string{"/private/tmp/a.sock"}}, []string{`(subpath "/Users/a")`, `(path-literal "/private/tmp/a.sock")`, "path-regex"}, []string{"mach-lookup", "file-read"}},
		{"auto without credential", CredentialDeny{Files: true, SSHAgent: true}, []string{"file-read*", "unix-socket"}, []string{"mach-lookup", "Keychains"}},
	}
	for _, r := range rows {
		got := render(r.d)
		for _, h := range r.has {
			if !strings.Contains(got, h) {
				t.Errorf("%s: render lacks %q:\n%s", r.name, h, got)
			}
		}
		for _, n := range r.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: render carries %q, want absent:\n%s", r.name, n, got)
			}
		}
	}
}

// TestProfile_CredentialDeny_FailClosed (#3792, control 7) pins that every
// non-canonical or unrenderable input and every non-not-exist resolver
// error returns an error naming the path and NO profile.
func TestProfile_CredentialDeny_FailClosed(t *testing.T) {
	eacces := func(p string) (string, error) {
		if p == "/Users/a/.ssh" {
			return "", &fs.PathError{Op: "lstat", Path: p, Err: syscall.EACCES}
		}
		return "", fs.ErrNotExist
	}
	rows := []struct {
		name string
		d    CredentialDeny
		err  string
	}{
		{"relative home", CredentialDeny{Homes: []string{"Users/a"}, Files: true}, `home "Users/a" is not absolute`},
		{"home with quote", CredentialDeny{Homes: []string{`/Users/a") (allow default`}, Files: true}, "contains a quote, backslash or control character"},
		{"home with backslash", CredentialDeny{Homes: []string{`/Users/a\b`}, Files: true}, "contains a quote, backslash or control character"},
		{"home with newline", CredentialDeny{Homes: []string{"/Users/a\n(allow default)"}, Files: true}, "contains a quote, backslash or control character"},
		{"unclean home", CredentialDeny{Homes: []string{"/Users/a/../b"}, Files: true}, `home "/Users/a/../b" is not a clean path`},
		{"trailing-slash home", CredentialDeny{Homes: []string{"/Users/a/"}, Files: true}, "is not a clean path"},
		{"relative socket", CredentialDeny{Homes: []string{"/Users/a"}, SSHAgent: true, AgentSockets: []string{"agent.sock"}}, `agent socket "agent.sock" is not absolute`},
		{"resolver EACCES", CredentialDeny{Homes: []string{"/Users/a"}, Files: true, EvalSymlinks: eacces}, `resolving credential path "/Users/a/.ssh"`},
		{"resolved target with quote", CredentialDeny{Homes: []string{"/Users/a"}, Files: true, EvalSymlinks: fakeResolver(map[string]string{
			"/Users/a/.ssh": `/x/s"sh`,
		})}, `resolved target of /Users/a/.ssh "/x/s\"sh" contains a quote`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.d.EvalSymlinks == nil {
				r.d.EvalSymlinks = fakeResolver(nil)
			}
			got, err := Profile("127.0.0.1:8090", nil, WithCredentialDeny(r.d))
			if err == nil || !strings.Contains(err.Error(), r.err) {
				t.Fatalf("err = %v, want containing %q (profile %q)", err, r.err, got)
			}
			if got != "" {
				t.Fatalf("profile must be empty on error, got %q", got)
			}
		})
	}
}

// TestSeatbelt_DeniesCredentialSurfaces_ProxyPathSurvives (#3792, control
// 8) is the darwin-only end-to-end for WithCredentialDeny: a REAL
// sandbox-exec against a hermetic fixture — two homes (the second with
// symlinked .ssh, .git-credentials and .config), an alias symlink, four
// listening unix sockets, a password-created unlocked keychain and the
// egress proxy. Every control is first asserted to SUCCEED unsandboxed, so
// none is vacuous.
func TestSeatbelt_DeniesCredentialSurfaces_ProxyPathSurvives(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt e2e is darwin-only (the Linux gap is the stated residual)")
	}
	for _, bin := range []string{"/usr/bin/curl", "/usr/bin/security", "/bin/cat"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("%s absent", bin)
		}
	}
	if avail, reason := Probe(context.Background()); !avail {
		t.Skipf("net sandbox unavailable on this host: %s", reason)
	}

	// AF_UNIX paths cap at 104 bytes on macOS, so the fixture lives under
	// /tmp, canonicalised (/tmp → /private/tmp) because homes must be.
	mkTemp := func(dir, pattern string) string {
		t.Helper()
		d, err := os.MkdirTemp(dir, pattern)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		r, err := filepath.EvalSymlinks(d)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	base := mkTemp("/tmp", "fhcred")
	h1, h2, elsewhere := filepath.Join(base, "h1"), filepath.Join(base, "h2"), filepath.Join(base, "elsewhere")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	symlink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(h1, ".ssh", "id_test"), "H1-SSH")
	write(filepath.Join(h1, ".config", "gh", "hosts.yml"), "H1-GH")
	write(filepath.Join(h1, ".git-credentials"), "H1-CREDS")
	write(filepath.Join(elsewhere, "ssh-dir", "id_test"), "H2-SSH")
	write(filepath.Join(elsewhere, "creds-file"), "H2-CREDS")
	write(filepath.Join(elsewhere, "config", "gh", "hosts.yml"), "H2-GH")
	if err := os.MkdirAll(h2, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink(filepath.Join(elsewhere, "ssh-dir"), filepath.Join(h2, ".ssh"))
	symlink(filepath.Join(elsewhere, "creds-file"), filepath.Join(h2, ".git-credentials"))
	symlink(filepath.Join(elsewhere, "config"), filepath.Join(h2, ".config"))
	symlink(filepath.Join(elsewhere, "ssh-dir"), filepath.Join(base, "alias-ssh"))

	// Four listening sockets, each serving HTTP 200: S1 under H1, S2 at the
	// declared agent path, S3 at the launchd Listeners pattern, S4 undeclared.
	serve := func(path string) {
		t.Helper()
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
		}), ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
	}
	s1 := filepath.Join(h1, "s1.sock")
	s2 := filepath.Join(base, "agent.sock")
	s3 := filepath.Join(mkTemp("/private/tmp", "com.apple.launchd.fh"), "Listeners")
	s4 := filepath.Join(base, "s4.sock")
	for _, s := range []string{s1, s2, s3, s4} {
		serve(s)
	}

	// Hermetic keychain under a throwaway HOME so the operator's search list
	// is never touched: password-created, unlocked, no auto-lock.
	const kcPass, kcSvc, kcSentinel = "fh-test-pw", "fishhawk-netsandbox-e2e", "KEYCHAIN-SENTINEL"
	kcHome := filepath.Join(base, "kchome")
	if err := os.MkdirAll(kcHome, 0o700); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(base, "fh.keychain")
	kcEnv := []string{"HOME=" + kcHome, "PATH=/usr/bin:/bin"}
	security := func(args ...string) {
		t.Helper()
		cmd := exec.Command("/usr/bin/security", args...)
		cmd.Env = kcEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("security %v: %v: %s", args, err, out)
		}
	}
	security("create-keychain", "-p", kcPass, kc)
	t.Cleanup(func() {
		cmd := exec.Command("/usr/bin/security", "delete-keychain", kc)
		cmd.Env = kcEnv
		_ = cmd.Run()
	})
	security("unlock-keychain", "-p", kcPass, kc)
	security("set-keychain-settings", kc)
	security("add-generic-password", "-A", "-a", "fh", "-s", kcSvc, "-w", kcSentinel, kc)

	var forgeHits atomic.Int32
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forgeHits.Add(1)
		w.WriteHeader(200)
	}))
	defer forge.Close()
	proxy, err := egressproxy.Start(egressproxy.Config{AllowHosts: []string{strings.TrimPrefix(forge.URL, "http://")}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Close() }()
	profile, err := Profile(strings.TrimPrefix(proxy.URL(), "http://"), nil, WithCredentialDeny(CredentialDeny{
		Homes: []string{h1, h2}, Files: true, Keychain: true, SSHAgent: true, AgentSockets: []string{s2},
	}))
	if err != nil {
		t.Fatal(err)
	}

	// run executes argv directly or under the profile and returns the exit
	// code and combined output.
	run := func(t *testing.T, sandboxed bool, env []string, argv ...string) (int, string) {
		t.Helper()
		if sandboxed {
			argv = Wrap(argv, profile)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out)
		} else if err != nil {
			t.Fatalf("spawn %v: %v", argv, err)
		}
		return 0, string(out)
	}
	plain := []string{"PATH=/usr/bin:/bin"}
	// deniedRead asserts path is readable unsandboxed (non-vacuous) and
	// unreadable under the profile.
	deniedRead := func(t *testing.T, path, sentinel string) {
		t.Helper()
		if code, out := run(t, false, plain, "/bin/cat", path); code != 0 || out != sentinel {
			t.Fatalf("unsandboxed cat %s: exit %d out %q, want 0/%q (fixture broken)", path, code, out, sentinel)
		}
		if code, out := run(t, true, plain, "/bin/cat", path); code == 0 || strings.Contains(out, sentinel) {
			t.Errorf("sandboxed cat %s: exit %d out %q, want refused", path, code, out)
		}
	}
	curlSock := func(t *testing.T, sandboxed bool, sock string) (int, string) {
		t.Helper()
		return run(t, sandboxed, plain, "/usr/bin/curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "-m", "5", "--unix-socket", sock, "http://fh/")
	}
	deniedSock := func(t *testing.T, sock string) {
		t.Helper()
		if code, out := curlSock(t, false, sock); code != 0 || out != "200" {
			t.Fatalf("unsandboxed connect %s: exit %d out %q, want 0/200 (fixture broken)", sock, code, out)
		}
		if code, out := curlSock(t, true, sock); code != 7 {
			t.Errorf("sandboxed connect %s: exit %d out %q, want curl exit 7 (connect refused)", sock, code, out)
		}
	}

	t.Run("a_keychain_mach_lookup_denied", func(t *testing.T) {
		argv := []string{"/usr/bin/security", "find-generic-password", "-s", kcSvc, "-w", kc}
		if code, out := run(t, false, kcEnv, argv...); code != 0 || strings.TrimSpace(out) != kcSentinel {
			t.Fatalf("unsandboxed keychain read: exit %d out %q, want 0/%s (fixture broken)", code, out, kcSentinel)
		}
		if code, out := run(t, true, kcEnv, argv...); code == 0 || strings.Contains(out, kcSentinel) {
			t.Errorf("sandboxed keychain read: exit %d out %q, want refused", code, out)
		}
	})
	t.Run("b_h1_credential_files_denied", func(t *testing.T) {
		deniedRead(t, filepath.Join(h1, ".ssh", "id_test"), "H1-SSH")
		deniedRead(t, filepath.Join(h1, ".config", "gh", "hosts.yml"), "H1-GH")
		deniedRead(t, filepath.Join(h1, ".git-credentials"), "H1-CREDS")
	})
	t.Run("c_socket_under_home_denied", func(t *testing.T) { deniedSock(t, s1) })
	t.Run("d_declared_agent_socket_denied", func(t *testing.T) { deniedSock(t, s2) })
	t.Run("e_launchd_listeners_socket_denied", func(t *testing.T) { deniedSock(t, s3) })
	t.Run("f_undeclared_socket_still_connects", func(t *testing.T) {
		if code, out := curlSock(t, true, s4); code != 0 || out != "200" {
			t.Errorf("sandboxed connect %s: exit %d out %q, want 0/200 (the deny is targeted, not all sockets)", s4, code, out)
		}
	})
	t.Run("g_proxy_path_survives", func(t *testing.T) {
		code, out := run(t, true, []string{"http_proxy=" + proxy.URL(), "PATH=/usr/bin"},
			"/usr/bin/curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "-m", "5", forge.URL+"/")
		if code != 0 || out != "200" {
			t.Errorf("forge via proxy under the credential profile: exit %d out %q, want 0/200", code, out)
		}
		if n := forgeHits.Load(); n != 1 {
			t.Errorf("forge hits = %d, want 1 (through the proxy)", n)
		}
	})
	t.Run("g2_admitted_port_survives_across_ports", func(t *testing.T) {
		// A unix-socket deny rendered AFTER the port allow denies the TCP
		// connect for a port-dependent ~15% of ports, so one port proves
		// nothing; 40 fresh ports make a regression all but certain to land.
		d := CredentialDeny{Homes: []string{h1, h2}, Files: true, Keychain: true, SSHAgent: true, AgentSockets: []string{s2}}
		for i := 0; i < 40; i++ {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
			p, err := Profile(strings.TrimPrefix(srv.URL, "http://"), nil, WithCredentialDeny(d))
			if err != nil {
				srv.Close()
				t.Fatal(err)
			}
			argv := Wrap([]string{"/usr/bin/curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-m", "5", srv.URL + "/"}, p)
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = plain
			out, err := cmd.CombinedOutput()
			srv.Close()
			if err != nil || string(out) != "200" {
				t.Fatalf("admitted port %s under the credential profile: err %v out %q, want 200", srv.URL, err, out)
			}
		}
	})
	t.Run("h_symlinked_credential_paths_denied", func(t *testing.T) {
		// (i) via the H2 home paths — .config is an INTERMEDIATE-component
		// symlink, the discriminating case for the home path itself.
		deniedRead(t, filepath.Join(h2, ".ssh", "id_test"), "H2-SSH")
		deniedRead(t, filepath.Join(h2, ".git-credentials"), "H2-CREDS")
		deniedRead(t, filepath.Join(h2, ".config", "gh", "hosts.yml"), "H2-GH")
		// (ii) the resolved targets directly.
		deniedRead(t, filepath.Join(elsewhere, "ssh-dir", "id_test"), "H2-SSH")
		deniedRead(t, filepath.Join(elsewhere, "creds-file"), "H2-CREDS")
		deniedRead(t, filepath.Join(elsewhere, "config", "gh", "hosts.yml"), "H2-GH")
		// (iii) through an alias symlink the profile never names.
		deniedRead(t, filepath.Join(base, "alias-ssh", "id_test"), "H2-SSH")
	})
}
