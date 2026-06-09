package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	cfg := config{backendURL: "http://localhost:8080", apiToken: token}
	newServer := func() *mcp.Server {
		srv := buildServer(cfg)
		registerTools(srv, &runResolver{api: newAPIClient(cfg), getenv: envFunc(nil)})
		return srv
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
	case <-time.After(httpShutdownTimeout + time.Second):
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

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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
