package netsandbox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
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
