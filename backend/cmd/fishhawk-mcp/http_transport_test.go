package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// setHTTPShutdownTimeout overrides the package-level httpShutdownTimeout for a
// timing-sensitive test and returns a restore closure — modeled on
// backend/internal/claudecode's setKillGrace. Swapping a package var is safe
// here ONLY because no test in package main calls t.Parallel(); a future
// parallel test would race this swap and must instead thread the timeout
// explicitly.
func setHTTPShutdownTimeout(d time.Duration) func() {
	prev := httpShutdownTimeout
	httpShutdownTimeout = d
	return func() { httpShutdownTimeout = prev }
}

func TestValidateLoopbackAddr(t *testing.T) {
	// stubLookup answers a fixed host->IPs map; any other host errors so
	// a test can't accidentally lean on real DNS.
	stubLookup := func(m map[string][]net.IP) func(string) ([]net.IP, error) {
		return func(host string) ([]net.IP, error) {
			ips, ok := m[host]
			if !ok {
				return nil, errors.New("no such host")
			}
			return ips, nil
		}
	}

	cases := []struct {
		name    string
		addr    string
		lookup  func(string) ([]net.IP, error)
		want    string
		wantErr bool
	}{
		{name: "ipv4 loopback", addr: "127.0.0.1:0", want: "127.0.0.1:0"},
		{name: "ipv6 loopback", addr: "[::1]:0", want: "[::1]:0"},
		{name: "empty host clamps to loopback", addr: ":8765", want: "127.0.0.1:8765"},
		{
			name:   "localhost resolving to loopback",
			addr:   "localhost:0",
			lookup: stubLookup(map[string][]net.IP{"localhost": {net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}),
			want:   "localhost:0",
		},
		{name: "reject 0.0.0.0", addr: "0.0.0.0:0", wantErr: true},
		{name: "reject routable literal", addr: "8.8.8.8:0", wantErr: true},
		{
			name:    "reject hostname resolving off-loopback",
			addr:    "evil.local:0",
			lookup:  stubLookup(map[string][]net.IP{"evil.local": {net.ParseIP("203.0.113.5")}}),
			wantErr: true,
		},
		{
			name:    "reject hostname with any non-loopback IP",
			addr:    "mixed.local:0",
			lookup:  stubLookup(map[string][]net.IP{"mixed.local": {net.ParseIP("127.0.0.1"), net.ParseIP("203.0.113.5")}}),
			wantErr: true,
		},
		{name: "missing port", addr: "127.0.0.1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup := tc.lookup
			if lookup == nil {
				lookup = func(string) ([]net.IP, error) {
					return nil, errors.New("lookup should not be called for a literal IP")
				}
			}
			got, err := validateLoopbackAddr(tc.addr, lookup)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateLoopbackAddr(%q) = %q, want error", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateLoopbackAddr(%q): %v", tc.addr, err)
			}
			if got != tc.want {
				t.Errorf("validateLoopbackAddr(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

func TestBearerAuthMiddleware(t *testing.T) {
	const token = "tok_secret_value"
	cases := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "valid token passes", header: "Bearer " + token, wantStatus: http.StatusOK},
		{name: "missing header", header: "", wantStatus: http.StatusUnauthorized},
		{name: "missing bearer prefix", header: token, wantStatus: http.StatusUnauthorized},
		{name: "wrong token same length", header: "Bearer tok_secret_VALUE", wantStatus: http.StatusUnauthorized},
		{name: "wrong token different length", header: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "empty bearer", header: "Bearer ", wantStatus: http.StatusUnauthorized},
	}

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	mw := bearerAuthMiddleware(next, token)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if reached {
					t.Error("next handler should not be reached on a 401")
				}
				if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
					t.Errorf("WWW-Authenticate = %q, want Bearer", got)
				}
				if body := rec.Body.String(); contains(body, token) {
					t.Errorf("401 body must not echo the expected token; got %q", body)
				}
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// bearerRoundTripper injects Authorization: Bearer <token> on every
// outbound request, so the MCP streamable client authenticates through
// the loopback bearer gate.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}

// TestServeHTTP_RoundTrip is the seam test: it boots the real
// StreamableHTTPHandler on a 127.0.0.1:0 listener, drives an actual MCP
// client over HTTP through the bearer middleware, and asserts ListTools
// returns the identical tool surface as the in-process server. It also
// asserts a bearer-less client is rejected.
func TestServeHTTP_RoundTrip(t *testing.T) {
	const token = "tok_roundtrip"

	// Route the graceful-shutdown timeout and this test's own outer guard
	// through one timescale base so both scale by a single factor and their
	// ratio (outer strictly greater than inner) holds at any factor — the
	// property that makes the 'serveHTTP did not return' branch below mean a
	// wedge rather than a slow runner (AGENTS.md wall-clock-boundary rule,
	// #2628). Capture the production default BEFORE overriding it.
	base := httpShutdownTimeout
	defer setHTTPShutdownTimeout(timescale.D(base))()
	// Build through the extracted cross-package entry point mcpserver.NewServer
	// (E66.7 / #2408) — the same construction path main.go's newServer now uses
	// — so this seam test exercises the binary->package boundary and the
	// onboarding resource still crosses the registration->transport seam.
	newServer := func() *mcp.Server {
		return mcpserver.NewServer(mcpserver.Config{BackendURL: "http://localhost:8080", APIToken: token})
	}

	// Bind a loopback listener up front so we know the port and can run
	// serveHTTP against the same addr (serveHTTP re-binds the validated
	// addr; close ours first to avoid a collision).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveHTTP(ctx, addr, token, newServer)
	}()

	url := "http://" + addr
	waitForListener(t, addr)

	// Expected surface from the in-process server, over an in-memory
	// transport — the same path TestToolDescriptions uses.
	wantTools := listToolNames(t, newServer())

	t.Run("authenticated client sees the full tool surface", func(t *testing.T) {
		httpClient := &http.Client{
			Transport: bearerRoundTripper{token: token, base: http.DefaultTransport},
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: httpClient,
		}, nil)
		if err != nil {
			t.Fatalf("connect with bearer: %v", err)
		}
		defer session.Close()

		res, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		got := make([]string, 0, len(res.Tools))
		for _, tool := range res.Tools {
			got = append(got, tool.Name)
		}
		if !sameStringSet(got, wantTools) {
			t.Errorf("HTTP tool surface = %v, want %v (must match stdio)", got, wantTools)
		}
	})

	t.Run("authenticated client reads the onboarding runbook over HTTP", func(t *testing.T) {
		httpClient := &http.Client{
			Transport: bearerRoundTripper{token: token, base: http.DefaultTransport},
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: httpClient,
		}, nil)
		if err != nil {
			t.Fatalf("connect with bearer: %v", err)
		}
		defer session.Close()

		// The instructions field also crosses the HTTP handshake.
		if got := session.InitializeResult().Instructions; !strings.Contains(got, "fishhawk_start_run") {
			t.Errorf("HTTP initialize instructions missing happy-path anchor; got %q", got)
		}

		list, err := session.ListResources(ctx, nil)
		if err != nil {
			t.Fatalf("ListResources over HTTP: %v", err)
		}
		found := false
		for _, r := range list.Resources {
			if r.URI == "fishhawk://runbook" {
				found = true
			}
		}
		if !found {
			t.Fatalf("HTTP ListResources did not include %s (resource did not cross the transport seam)", "fishhawk://runbook")
		}

		res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "fishhawk://runbook"})
		if err != nil {
			t.Fatalf("ReadResource over HTTP: %v", err)
		}
		if len(res.Contents) == 0 || strings.TrimSpace(res.Contents[0].Text) == "" {
			t.Fatal("HTTP runbook read returned empty content")
		}
		if !strings.Contains(res.Contents[0].Text, "runner_kind:local") {
			t.Error("HTTP runbook content missing the runner_kind:local edge case")
		}
	})

	t.Run("no-bearer client is rejected", func(t *testing.T) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: http.DefaultClient,
		}, nil)
		if err == nil {
			// Some clients surface the 401 on the first call rather than
			// Connect; treat either as a rejection.
			_, lerr := session.ListTools(ctx, nil)
			session.Close()
			if lerr == nil {
				t.Fatal("expected a no-bearer client to be rejected with 401")
			}
		}
	})

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("serveHTTP returned %v after cancel", err)
		}
	// Outer guard derives from the SAME base as the overridden inner shutdown
	// timeout (timescale.D), so it stays strictly greater than it at any factor.
	case <-time.After(timescale.D(base + time.Second)):
		t.Error("serveHTTP did not return after ctx cancel")
	}
}

func TestServeHTTP_RejectsNonLoopbackAddr(t *testing.T) {
	// A non-loopback addr must fail before any bind.
	err := serveHTTP(context.Background(), "0.0.0.0:0", "tok", func() *mcp.Server { return nil })
	if err == nil {
		t.Fatal("expected serveHTTP to reject a non-loopback addr")
	}
}

// TestServeHTTP_ShutdownTimeoutFiresOnWedgedConn is the discrimination test for
// #2628: it proves the graceful-shutdown bound still FIRES on a genuinely
// wedged shutdown rather than merely having been enlarged to always pass.
//
// The wedge is a deterministically ACTIVE connection: a fully-received tool
// call whose handler blocks. net/http.Server.Shutdown closes idle connections
// but WAITS for active ones, so while the handler blocks the connection stays
// in StateActive and Shutdown cannot complete — it hits the (scaled, short)
// shutdown deadline and serveHTTP returns the context.DeadlineExceeded that
// srv.Shutdown yields verbatim on ctx.Done and serveHTTP propagates unwrapped.
//
// The counterfactual is the block itself (see the marked select): remove it and
// the handler returns immediately, the connection returns to idle, Shutdown
// reaps it, and serveHTTP returns nil — flipping the DeadlineExceeded assertion
// RED. The standalone SSE GET is disabled on the client so the ONLY active
// connection is the tool-call POST, keeping the block load-bearing (a bare or
// partial connection would sit in StateNew, which Shutdown also refuses to reap
// under 5s, and so would NOT discriminate the block).
func TestServeHTTP_ShutdownTimeoutFiresOnWedgedConn(t *testing.T) {
	const token = "tok_wedge"

	// A deliberately SHORT scaled shutdown timeout: this test consumes it in
	// full, so keep the base small (200ms → 1s at CI factor 5).
	base := 200 * time.Millisecond
	defer setHTTPShutdownTimeout(timescale.D(base))()

	entered := make(chan struct{}) // the named observable: handler reached
	release := make(chan struct{}) // closed in cleanup to unblock the handler
	var enterOnce sync.Once

	newServer := func() *mcp.Server {
		srv := mcp.NewServer(&mcp.Implementation{Name: "wedge", Version: "0"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "block", Description: "blocks until released"},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
				enterOnce.Do(func() { close(entered) })
				// THE CONTROL: hold the request open so the connection stays
				// StateActive and Shutdown cannot reap it. Deleting this select
				// is the counterfactual — the handler then returns immediately,
				// the connection goes idle, Shutdown returns nil, and the
				// DeadlineExceeded assertion below goes RED.
				select {
				case <-release:
				case <-ctx.Done():
				}
				return nil, struct{}{}, nil
			})
		return srv
	}

	// Bind a loopback port to learn an addr, then close it so serveHTTP can
	// re-bind the same addr (as TestServeHTTP_RoundTrip does).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveHTTP(ctx, addr, token, newServer)
	}()
	waitForListener(t, addr)

	// Drive a blocking tool call in the background. Its request context is
	// context.Background(), NOT the serveHTTP ctx — cancelling serveHTTP must
	// not tear down the in-flight request, or the connection would leave
	// StateActive and the wedge would dissolve.
	sessionCh := make(chan *mcp.ClientSession, 1)
	connectErr := make(chan error, 1)
	go func() {
		httpClient := &http.Client{
			Transport: bearerRoundTripper{token: token, base: http.DefaultTransport},
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "wedge-client", Version: "0"}, nil)
		session, cerr := client.Connect(context.Background(), &mcp.StreamableClientTransport{
			Endpoint:             "http://" + addr,
			HTTPClient:           httpClient,
			DisableStandaloneSSE: true,
		}, nil)
		if cerr != nil {
			connectErr <- cerr
			return
		}
		sessionCh <- session
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "block"})
	}()

	// Cleanup: unblock the handler and close the session so no goroutine leaks.
	t.Cleanup(func() {
		close(release)
		select {
		case session := <-sessionCh:
			_ = session.Close()
		default:
		}
	})

	// Wait on the observable — the handler was reached, so the request is fully
	// received and the connection is StateActive — before cancelling. No sleep.
	select {
	case <-entered:
	case cerr := <-connectErr:
		t.Fatalf("client connect: %v", cerr)
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("blocking tool handler was never entered")
	}

	cancel() // trigger the graceful-shutdown path with the wedge in place

	select {
	case err := <-serveErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serveHTTP = %v, want context.DeadlineExceeded from the wedged shutdown", err)
		}
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("serveHTTP did not return within the scaled shutdown budget")
	}
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	// The liveness deadline is a spawn/reap wait, so it scales with the factor
	// (AGENTS.md). The 10ms poll interval and 100ms per-attempt dial timeout are
	// sub-boundary sampling granularity, not assertion bounds, and stay unscaled:
	// a dial that times out simply retries until the scaled deadline.
	deadline := time.Now().Add(timescale.D(3 * time.Second))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener at %s never came up", addr)
}

func listToolNames(t *testing.T, srv *mcp.Server) []string {
	t.Helper()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()
	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
