package egressproxy_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/egressproxy"
)

// proxiedClient returns an http.Client routed through the proxy, trusting
// the test TLS server's certificate handling via InsecureSkipVerify (the
// tunnel is opaque to the proxy; the client terminates TLS).
func proxiedClient(t *testing.T, p *egressproxy.Proxy) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(p.URL())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test client against httptest's self-signed cert
		},
		Timeout: 10 * time.Second,
	}
}

func startProxy(t *testing.T, cfg egressproxy.Config) *egressproxy.Proxy {
	t.Helper()
	p, err := egressproxy.Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestConnect_AllowedHostTunnels proves the happy path: a TLS request to an
// allow-listed host:port tunnels through CONNECT end-to-end.
func TestConnect_AllowedHostTunnels(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "tunneled ok")
	}))
	defer backend.Close()

	backendHost := strings.TrimPrefix(backend.URL, "https://")
	p := startProxy(t, egressproxy.Config{AllowHosts: []string{backendHost}})

	resp, err := proxiedClient(t, p).Get(backend.URL)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tunneled ok" {
		t.Errorf("body = %q, want %q", body, "tunneled ok")
	}
}

// TestConnect_DeniedHost403 proves default-deny: a CONNECT to a host absent
// from the allow-list is refused with 403 before any upstream dial.
func TestConnect_DeniedHost403(t *testing.T) {
	p := startProxy(t, egressproxy.Config{AllowHosts: []string{"allowed.example.test"}})

	_, err := proxiedClient(t, p).Get("https://denied.example.test/")
	if err == nil {
		t.Fatal("GET to denied host succeeded, want CONNECT refusal")
	}
	if !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("error = %v, want a 403/Forbidden CONNECT refusal", err)
	}
}

// TestPlainHTTP_AllowedForwardsAndDeniedRefused covers the absolute-form
// plain-HTTP path for both decisions.
func TestPlainHTTP_AllowedForwardsAndDeniedRefused(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "forwarded ok")
	}))
	defer backend.Close()
	backendHost := strings.TrimPrefix(backend.URL, "http://")

	p := startProxy(t, egressproxy.Config{AllowHosts: []string{backendHost}})
	client := proxiedClient(t, p)

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("GET allowed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "forwarded ok" {
		t.Errorf("body = %q, want %q", body, "forwarded ok")
	}

	resp, err = client.Get("http://denied.example.test/")
	if err != nil {
		t.Fatalf("GET denied (plain HTTP forwards report status, not client error): %v", err)
	}
	deniedBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(deniedBody), "denied.example.test") {
		t.Errorf("denial body should name the denied host, got: %s", deniedBody)
	}
}

// TestHostOnlyEntry_DefaultPortsOnly proves a port-less entry admits only
// the default HTTP/HTTPS ports: the same host on a non-default port is
// denied (403), while the default port passes the allow decision (the dial
// then fails, surfacing as 502 — a different failure class than denial).
func TestHostOnlyEntry_DefaultPortsOnly(t *testing.T) {
	resolves := func(_ context.Context, _ string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.0.2.1")}, nil // TEST-NET-1, never routable
	}
	p := startProxy(t, egressproxy.Config{
		AllowHosts:  []string{"hostonly.example.test"},
		LookupIP:    resolves,
		DialTimeout: 200 * time.Millisecond,
	})
	client := proxiedClient(t, p)

	_, err := client.Get("https://hostonly.example.test:9999/")
	if err == nil || (!strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "Forbidden")) {
		t.Errorf("non-default port: err = %v, want 403 denial", err)
	}

	_, err = client.Get("https://hostonly.example.test/") // port 443: allowed, dial to TEST-NET-1 fails
	if err == nil {
		t.Fatal("dial to TEST-NET-1 unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("default port should pass the allow decision (failure must be the dial, not a 403): %v", err)
	}
}

// TestDNSPinning_FirstResolutionWins proves anti-rebinding pinning: the
// resolver is consulted once per hostname and later requests reuse the
// pinned answer even when the resolver would now answer differently.
func TestDNSPinning_FirstResolutionWins(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "pinned backend")
	}))
	defer backend.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))

	var calls atomic.Int32
	lookup := func(_ context.Context, _ string) ([]net.IP, error) {
		if calls.Add(1) > 1 {
			// A post-pin rebind attempt: different (unroutable) answer.
			return []net.IP{net.ParseIP("192.0.2.99")}, nil
		}
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	p := startProxy(t, egressproxy.Config{
		AllowHosts:  []string{"pinned.example.test:" + portStr},
		LookupIP:    lookup,
		DialTimeout: 200 * time.Millisecond,
	})

	// The pinned IP (203.0.113.7, TEST-NET-3) is not actually dialable, so
	// assert pinning via resolver call count across repeated requests.
	client := proxiedClient(t, p)
	for i := 0; i < 3; i++ {
		_, _ = client.Get("http://pinned.example.test:" + portStr + "/")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("resolver calls = %d, want 1 (resolution must be pinned for the proxy lifetime)", got)
	}
}

// TestRebindingShapedResolution_Refused proves the second anti-rebinding
// layer: a public hostname resolving into loopback/private space is refused
// outright rather than dialed.
func TestRebindingShapedResolution_Refused(t *testing.T) {
	for _, tc := range []struct{ name, ip string }{
		{"loopback", "127.0.0.1"},
		{"private", "10.1.2.3"},
		{"link-local", "169.254.1.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(_ context.Context, _ string) ([]net.IP, error) {
				return []net.IP{net.ParseIP(tc.ip)}, nil
			}
			p := startProxy(t, egressproxy.Config{
				AllowHosts: []string{"public.example.test"},
				LookupIP:   lookup,
			})
			_, err := proxiedClient(t, p).Get("https://public.example.test/")
			if err == nil {
				t.Fatal("rebinding-shaped resolution was dialed, want refusal")
			}
			if !strings.Contains(err.Error(), "502") && !strings.Contains(err.Error(), "Bad Gateway") {
				t.Errorf("err = %v, want the 502 refusal from the rebinding check", err)
			}
		})
	}
}

// TestLocalhostEntry_DialedAsDeclared proves an operator-declared localhost
// target (the dev-loop case) is admitted and dialed — the rebinding refusal
// applies to public hostnames, not to literal local declarations.
func TestLocalhostEntry_DialedAsDeclared(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "local target ok")
	}))
	defer backend.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))

	p := startProxy(t, egressproxy.Config{AllowHosts: []string{"localhost:" + portStr}})
	resp, err := proxiedClient(t, p).Get("http://localhost:" + portStr + "/")
	if err != nil {
		t.Fatalf("GET localhost target: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "local target ok" {
		t.Errorf("body = %q, want %q", body, "local target ok")
	}
}

// TestStart_RejectsMalformedEntries proves fail-closed configuration: URL
// and empty entries are Start-time errors, not silently-skipped rows.
func TestStart_RejectsMalformedEntries(t *testing.T) {
	for _, bad := range []string{"https://host.example.test", "host/path", ""} {
		if _, err := egressproxy.Start(egressproxy.Config{AllowHosts: []string{bad}}); err == nil {
			t.Errorf("Start with entry %q succeeded, want error", bad)
		}
	}
	if _, err := egressproxy.Start(egressproxy.Config{}); err == nil {
		t.Error("Start with an empty allow-list succeeded, want error")
	}
}

// TestBuildAllowlist composes the three ADR-050 classes and skips a
// malformed backend URL.
func TestBuildAllowlist(t *testing.T) {
	got := egressproxy.BuildAllowlist([]string{"staging.example.com:8443"}, "https://fishhawk.example.com:8080")
	want := map[string]bool{
		"staging.example.com:8443":  true,
		"api.anthropic.com":         true,
		"api.openai.com":            true,
		"fishhawk.example.com:8080": true,
	}
	if len(got) != len(want) {
		t.Fatalf("BuildAllowlist = %v, want the 4 composed entries", got)
	}
	for _, h := range got {
		if !want[h] {
			t.Errorf("unexpected allow-list entry %q", h)
		}
	}
	if entries := egressproxy.BuildAllowlist(nil, "::not-a-url::"); len(entries) != len(egressproxy.DefaultModelHosts) {
		t.Errorf("malformed backend URL must be skipped, got %v", entries)
	}
}

// TestClose_Idempotent proves double-Close is safe (the invocation teardown
// path may race the deferred cleanup).
func TestClose_Idempotent(t *testing.T) {
	p := startProxy(t, egressproxy.Config{AllowHosts: []string{"h.example.test"}})
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
