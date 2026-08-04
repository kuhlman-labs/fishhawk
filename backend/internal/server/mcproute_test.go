package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// mcpTestTokens issues one live bearer against the package's existing
// apitoken.Repository fake and returns (repo, plaintext). Reusing the shared
// fake — rather than a bespoke stub — keeps these tests on the same
// hash-and-look-up path production authenticates through, so an unlisted
// plaintext is an ErrNotFound that bearerAuth resolves to the anonymous
// identity (the unknown-bearer case).
func mcpTestTokens(t *testing.T, scopes ...string) (*fakeTokenRepo, string) {
	t.Helper()
	repo := newFakeTokenRepo()
	if len(scopes) == 0 {
		scopes = []string{"read:runs", "write:runs"}
	}
	tok, err := repo.Issue(context.Background(), "svc:mcp-test", scopes)
	if err != nil {
		t.Fatalf("issue test token: %v", err)
	}
	return repo, tok.PlainText
}

// mcpTestSession signs a cookie session into the package's auth.Repository
// fake and returns (repo, cookie plaintext). Driving the cookie branch
// through a REAL resolved session — SessionID set, TokenID empty — is what
// makes the cookie-refusal assertion meaningful.
func mcpTestSession(t *testing.T) (*fakeAuthRepo, string) {
	t.Helper()
	repo := newFakeAuthRepo()
	_, sess, err := repo.SignIn(context.Background(), "github",
		auth.GitHubProfile{ID: 42, Login: "operator"}, uuid.Nil)
	if err != nil {
		t.Fatalf("sign in test session: %v", err)
	}
	return repo, sess.PlainText
}

// mcpTestServer builds a Server from cfg with a live bearer wired, and
// returns the server plus that bearer's plaintext.
func mcpTestServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	repo, plain := mcpTestTokens(t)
	cfg.APITokenRepo = repo
	if cfg.MCPServerFactory == nil {
		cfg.MCPServerFactory = testMCPServerFactory
	}
	return New(cfg), plain
}

// testMCPServerFactory is the production wiring serve.go installs, restated
// here because internal/server deliberately does not import mcpserver (that
// edge would close an import cycle in mcpserver's test binary). Tests DO
// import it, which is legal in the other direction and is what keeps the
// registry-parity assertion honest.
func testMCPServerFactory(backendURL, apiToken string) *mcp.Server {
	return mcpserver.NewServer(mcpserver.Config{BackendURL: backendURL, APIToken: apiToken})
}

// newMCPPost builds a streamable-HTTP POST the SDK will accept: the
// Content-Type and dual Accept headers it validates before any routing.
func newMCPPost(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return req
}

// mcpInitializeBody is a minimal, valid JSON-RPC initialize request.
const mcpInitializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
	`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// loopbackLookup answers every hostname with 127.0.0.1.
func loopbackLookup(string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }

// ---------------------------------------------------------------------------
// Pure predicates
// ---------------------------------------------------------------------------

// TestMCPLoopbackHost tables the loopback predicate. The EMPTY-host row is
// the load-bearing one: `:8080` (the shipped FISHHAWKD_ADDR default) binds
// every interface per net.Listen's documented behaviour, so it must be
// reported non-loopback and NOT clamped to 127.0.0.1.
func TestMCPLoopbackHost(t *testing.T) {
	lookupErr := errors.New("dns is down")
	mixed := func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.0.2.7")}, nil
	}
	allLoopback := func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("::1")}, nil
	}
	failing := func(string) ([]net.IP, error) { return nil, lookupErr }
	empty := func(string) ([]net.IP, error) { return nil, nil }

	tests := []struct {
		name     string
		host     string
		lookup   func(string) ([]net.IP, error)
		wantIP   string
		wantOK   bool
		wantErr  bool
		errMatch string
	}{
		{name: "ipv4 loopback literal", host: "127.0.0.1", wantIP: "127.0.0.1", wantOK: true},
		{name: "ipv6 loopback literal", host: "::1", wantIP: "::1", wantOK: true},
		{name: "non-canonical ipv4 loopback", host: "127.0.0.2", wantIP: "127.0.0.2", wantOK: true},
		{name: "unspecified ipv4", host: "0.0.0.0"},
		{name: "public ipv4", host: "192.0.2.1"},
		{name: "empty host is NOT clamped", host: ""},
		{name: "hostname all loopback", host: "localhost", lookup: allLoopback, wantIP: "127.0.0.2", wantOK: true},
		{name: "hostname mixed loopback", host: "sneaky.test", lookup: mixed},
		{name: "hostname lookup error", host: "broken.test", lookup: failing, wantErr: true, errMatch: "dns is down"},
		{name: "hostname resolves to nothing", host: "void.test", lookup: empty, wantErr: true, errMatch: "no addresses"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotIP, gotOK, err := mcpLoopbackHost(tc.host, tc.lookup)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want an error")
				}
				if !strings.Contains(err.Error(), tc.errMatch) {
					t.Errorf("err = %v, want it to mention %q", err, tc.errMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotIP != tc.wantIP {
				t.Errorf("ip = %q, want %q", gotIP, tc.wantIP)
			}
		})
	}
}

// TestMCPSelfURLDerivation tables the derived base URL. The IPv6 row is the
// reason net.JoinHostPort is used instead of concatenation: "::1"+":8080"
// would produce the unparseable "http://::1:8080".
func TestMCPSelfURLDerivation(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		lookup   func(string) ([]net.IP, error)
		want     string
		wantHost string
		wantPort string
	}{
		{name: "ipv4", addr: "127.0.0.1:8080", want: "http://127.0.0.1:8080", wantHost: "127.0.0.1", wantPort: "8080"},
		{name: "ipv6 is bracketed", addr: "[::1]:8080", want: "http://[::1]:8080", wantHost: "::1", wantPort: "8080"},
		{name: "ephemeral port", addr: "127.0.0.1:0", want: "http://127.0.0.1:0", wantHost: "127.0.0.1", wantPort: "0"},
		{
			name: "hostname is pinned to its resolved ip",
			addr: "localhost:9000", lookup: loopbackLookup,
			want: "http://127.0.0.1:9000", wantHost: "127.0.0.1", wantPort: "9000",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := resolveMCPRouteState(Config{Addr: tc.addr, MCPServerFactory: testMCPServerFactory}, tc.lookup)
			if st.mode != mcpRouteEnabled {
				t.Fatalf("mode = %v, want mcpRouteEnabled", st.mode)
			}
			if st.selfURL != tc.want {
				t.Fatalf("selfURL = %q, want %q", st.selfURL, tc.want)
			}
			u, err := url.Parse(st.selfURL)
			if err != nil {
				t.Fatalf("derived selfURL does not round-trip through url.Parse: %v", err)
			}
			if u.Hostname() != tc.wantHost || u.Port() != tc.wantPort {
				t.Errorf("parsed host/port = %q/%q, want %q/%q", u.Hostname(), u.Port(), tc.wantHost, tc.wantPort)
			}
		})
	}
}

// TestResolveMCPRouteState_PinsResolvedLoopbackIP is the CONDITION-3 pin: a
// hostname that resolves to a loopback address at construction and to a
// PUBLIC address afterwards must leave a self URL carrying the loopback IP
// literal. Storing the hostname would let the HTTP client re-resolve at call
// time and carry every caller's raw bearer off-host.
//
// The end-to-end companion — the loopback call actually ARRIVING at the
// pinned IP — is TestMCPRoute_SelfURLPinnedToResolvedIP in
// mcproute_integration_test.go.
func TestResolveMCPRouteState_PinsResolvedLoopbackIP(t *testing.T) {
	var mu sync.Mutex
	flipped := false
	lookup := func(string) ([]net.IP, error) {
		mu.Lock()
		defer mu.Unlock()
		if flipped {
			return []net.IP{net.ParseIP("203.0.113.9")}, nil
		}
		flipped = true
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}

	st := resolveMCPRouteState(Config{Addr: "flipflop.test:8080", MCPServerFactory: testMCPServerFactory}, lookup)
	if st.mode != mcpRouteEnabled {
		t.Fatalf("mode = %v, want mcpRouteEnabled at construction", st.mode)
	}
	if st.selfURL != "http://127.0.0.1:8080" {
		t.Fatalf("selfURL = %q, want the pinned loopback IP http://127.0.0.1:8080", st.selfURL)
	}
	if strings.Contains(st.selfURL, "flipflop.test") {
		t.Errorf("selfURL retained the hostname %q — the HTTP client would re-resolve it at call time", st.selfURL)
	}
	// Prove the DNS record really did flip, so the assertion above is about
	// pinning and not about a resolver that never changed.
	after, _ := lookup("flipflop.test")
	if len(after) != 1 || after[0].IsLoopback() {
		t.Fatalf("post-construction lookup = %v, want a single non-loopback address", after)
	}
}

// TestResolveMCPRouteState_PinsListenAddr is the listener half of the same
// resolution-to-use gap: pinning only the self URL leaves net.Listen to
// re-resolve the HOSTNAME in Config.Addr at Start, so a DNS record changed
// between New() and Start would bind the bare-bearer tool surface to an
// off-host interface the route still classifies as loopback.
//
// The end-to-end companion — Start actually binding the pinned literal — is
// TestMCPRoute_StartBindsPinnedLoopbackIP below.
func TestResolveMCPRouteState_PinsListenAddr(t *testing.T) {
	st := resolveMCPRouteState(
		Config{Addr: "flipflop.test:8080", MCPServerFactory: testMCPServerFactory},
		loopbackLookup)
	if st.mode != mcpRouteEnabled {
		t.Fatalf("mode = %v, want mcpRouteEnabled", st.mode)
	}
	if st.listenAddr != "127.0.0.1:8080" {
		t.Fatalf("listenAddr = %q, want the pinned 127.0.0.1:8080", st.listenAddr)
	}
	if strings.Contains(st.listenAddr, "flipflop.test") {
		t.Errorf("listenAddr retained the hostname %q — net.Listen would re-resolve it at bind time", st.listenAddr)
	}

	// An IPv6 loopback literal must stay bracketed, or the bind address is
	// unparseable.
	v6 := resolveMCPRouteState(
		Config{Addr: "[::1]:8080", MCPServerFactory: testMCPServerFactory}, nil)
	if v6.listenAddr != "[::1]:8080" {
		t.Errorf("ipv6 listenAddr = %q, want [::1]:8080", v6.listenAddr)
	}
}

// TestMCPRoute_ListenAddrUntouchedWhenRouteNotServed asserts the pinning is
// confined to a served route: a deployment the route refuses (or has
// switched off) must bind EXACTLY the address it configured, so this change
// cannot move a non-MCP deployment's listener.
func TestMCPRoute_ListenAddrUntouchedWhenRouteNotServed(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"non-loopback listener (403)", Config{Addr: "0.0.0.0:8080"}},
		{"route off", Config{Addr: "127.0.0.1:8080", MCPRoute: "off"}},
		{"default empty host", Config{Addr: ":8080"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := mcpTestServer(t, tc.cfg)
			if s.mcpRoute.mode == mcpRouteEnabled {
				t.Fatalf("mode = %v; this case is supposed to NOT serve the route", s.mcpRoute.mode)
			}
			if got := s.listenAddr(); got != tc.cfg.Addr {
				t.Errorf("listenAddr() = %q, want the configured %q untouched", got, tc.cfg.Addr)
			}
		})
	}
}

// TestMCPRoute_StartBindsPinnedLoopbackIP drives the REAL bind: Config.Addr
// names a hostname whose injected resolver answers loopback at construction
// and TEST-NET-3 (203.0.113.9, assignable on no interface) afterwards.
//
// If Start handed net.Listen the hostname, the bind would re-resolve and
// fail with "cannot assign requested address" — and in production it would
// instead bind whatever the new record named, exposing the bare-bearer tool
// surface off-host behind a route that still reported itself loopback-only.
// A clean bind on the pinned literal is the proof.
func TestMCPRoute_StartBindsPinnedLoopbackIP(t *testing.T) {
	var mu sync.Mutex
	construction := true
	lookup := func(string) ([]net.IP, error) {
		mu.Lock()
		defer mu.Unlock()
		if construction {
			construction = false
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("203.0.113.9")}, nil
	}

	s, _ := mcpTestServer(t, Config{Addr: "flipflop.test:0", mcpLookupIP: lookup})
	if s.mcpRoute.mode != mcpRouteEnabled {
		t.Fatalf("mode = %v (reason %q), want mcpRouteEnabled", s.mcpRoute.mode, s.mcpRoute.reason)
	}
	if got := s.listenAddr(); got != "127.0.0.1:0" {
		t.Fatalf("listenAddr() = %q, want the pinned 127.0.0.1:0", got)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	// Shutdown is safe whether or not Serve has begun — a server already
	// shutting down makes Serve return ErrServerClosed, which Start reports
	// as a clean stop. What it cannot mask is the BIND: Start calls
	// net.Listen before Serve either way, so a re-resolving bind surfaces as
	// a non-nil Start error here.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Start = %v; the bind re-resolved the hostname instead of using the pinned loopback literal", err)
	}

	// Prove the record really did flip, so the assertion above is about
	// pinning rather than a resolver that never changed.
	after, _ := lookup("flipflop.test")
	if len(after) != 1 || after[0].IsLoopback() {
		t.Fatalf("post-construction lookup = %v, want a single non-loopback address", after)
	}
}

// stringAddr is a net.Addr whose String() is whatever the test needs —
// including a value net.SplitHostPort cannot parse, which no real
// net.TCPAddr produces.
type stringAddr string

func (stringAddr) Network() string  { return "tcp" }
func (a stringAddr) String() string { return string(a) }

// TestVerifyMCPListenerLoopback pins the bind-time assertion. It is the last
// line of the loopback-only enforcement: if the address actually bound ever
// disagrees with the verdict resolved at New(), the whole listener fails
// closed rather than serving a bare-bearer tool surface off-host.
func TestVerifyMCPListenerLoopback(t *testing.T) {
	enabled := mcpRouteState{mode: mcpRouteEnabled, listenAddr: "127.0.0.1:8080"}
	tests := []struct {
		name    string
		state   mcpRouteState
		bound   net.Addr
		wantErr string // empty => want nil
	}{
		{name: "ipv4 loopback", state: enabled, bound: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}},
		{name: "ipv6 loopback", state: enabled, bound: &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8080}},
		{
			name: "public bind refuses", state: enabled,
			bound:   &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 8080},
			wantErr: "reachable off-host",
		},
		{
			name: "unspecified bind refuses", state: enabled,
			bound:   &net.TCPAddr{IP: net.IPv4zero, Port: 8080},
			wantErr: "reachable off-host",
		},
		{name: "unparseable bind refuses", state: enabled, bound: stringAddr("nonsense"), wantErr: "not a valid host:port"},
		{name: "nil addr refuses", state: enabled, wantErr: "no address"},
		// Every non-enabled verdict already refuses at the handler, so the
		// listener carries no MCP surface to protect and must not be gated.
		{name: "route off ignores the bind", state: mcpRouteState{mode: mcpRouteOff},
			bound: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 8080}},
		{name: "non-loopback verdict ignores the bind", state: mcpRouteState{mode: mcpRouteNotLoopback},
			bound: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 8080}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyMCPListenerLoopback(tc.state, tc.bound)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want a refusal mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestResolveMCPRouteState_SelfURLValidation tables pinLoopbackSelfURL through
// resolveMCPRouteState: every rejected shape is a 503 diagnosis, and an
// accepted one is rewritten to its resolved loopback IP.
func TestResolveMCPRouteState_SelfURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		selfURL string
		lookup  func(string) ([]net.IP, error)
		wantURL string // non-empty => expect mcpRouteEnabled with this selfURL
		wantMsg string // non-empty => expect mcpRouteMisconfigured mentioning this
	}{
		{name: "explicit loopback literal", selfURL: "http://127.0.0.1:7777", wantURL: "http://127.0.0.1:7777"},
		// https with an IP LITERAL rewrites nothing, so a certificate
		// carrying that IP SAN still verifies — accepted.
		{name: "https loopback literal", selfURL: "https://127.0.0.1:7777", wantURL: "https://127.0.0.1:7777"},
		// https with a HOSTNAME is the construction-passes/every-call-fails
		// shape: pinning replaces the name with an IP literal, so TLS would
		// be negotiated against a certificate issued for a name the dialled
		// host no longer carries. Refused at New(), not at handshake time.
		{
			name:    "https hostname is refused",
			selfURL: "https://localhost:7777", lookup: loopbackLookup,
			wantMsg: "would fail in the handshake",
		},
		{
			name:    "https hostname without a port is refused",
			selfURL: "https://mcp.internal", lookup: loopbackLookup,
			wantMsg: "pinned to its resolved loopback address",
		},
		{name: "ipv6 loopback keeps its brackets", selfURL: "http://[::1]:7777", wantURL: "http://[::1]:7777"},
		{name: "no port", selfURL: "http://127.0.0.1", wantURL: "http://127.0.0.1"},
		{
			name:    "hostname pinned to resolved ip",
			selfURL: "http://localhost:7777", lookup: loopbackLookup,
			wantURL: "http://127.0.0.1:7777",
		},
		{name: "public host", selfURL: "https://evil.example.com", lookup: func(string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.5")}, nil
		}, wantMsg: "is not a loopback address"},
		{name: "unsupported scheme", selfURL: "ftp://127.0.0.1:7777", wantMsg: `scheme "ftp"`},
		{name: "no host", selfURL: "http:///v0", wantMsg: "no host"},
		{name: "unparseable", selfURL: "http://[::1", wantMsg: "parse"},
		{name: "lookup failure", selfURL: "http://flaky.test:7777", lookup: func(string) ([]net.IP, error) {
			return nil, errors.New("resolver timeout")
		}, wantMsg: "resolver timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := resolveMCPRouteState(Config{Addr: "127.0.0.1:8080", MCPSelfURL: tc.selfURL, MCPServerFactory: testMCPServerFactory}, tc.lookup)
			if tc.wantURL != "" {
				if st.mode != mcpRouteEnabled {
					t.Fatalf("mode = %v (reason %q), want mcpRouteEnabled", st.mode, st.reason)
				}
				if st.selfURL != tc.wantURL {
					t.Errorf("selfURL = %q, want %q", st.selfURL, tc.wantURL)
				}
				return
			}
			if st.mode != mcpRouteMisconfigured {
				t.Fatalf("mode = %v, want mcpRouteMisconfigured", st.mode)
			}
			if !strings.Contains(st.reason, tc.wantMsg) {
				t.Errorf("reason = %q, want it to mention %q", st.reason, tc.wantMsg)
			}
			if !strings.Contains(st.reason, "off-host") {
				t.Errorf("reason = %q, want it to name the bearer-forwarding risk", st.reason)
			}
		})
	}
}

// TestNormalizeMCPRoute pins the on/off normalization the zero-value Config
// depends on. Strict rejection of an unrecognized operator value is
// fishhawkd's job (TestResolveMCPRouteMode in cmd/fishhawkd); this layer is
// deliberately permissive so an embedder's zero Config still serves.
func TestNormalizeMCPRoute(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "on"}, {"on", "on"}, {"ON", "on"}, {"off", "off"},
		{"OFF", "off"}, {" off ", "off"}, {"anything-else", "on"},
	} {
		if got := normalizeMCPRoute(tc.in); got != tc.want {
			t.Errorf("normalizeMCPRoute(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Refusal ladder, through the route
// ---------------------------------------------------------------------------

// TestMCPRoute_Off_404 asserts the explicit opt-out on ALL THREE methods —
// registering every leg on handleMCP is what makes a disabled route report
// itself consistently instead of one leg 404ing as "unrouted".
func TestMCPRoute_Off_404(t *testing.T) {
	s, token := mcpTestServer(t, Config{Addr: "127.0.0.1:8080", MCPRoute: "off"})
	for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			var req *http.Request
			if m == http.MethodPost {
				req = newMCPPost(mcpInitializeBody)
			} else {
				req = httptest.NewRequest(m, "/mcp", nil)
				req.Header.Set("Accept", "application/json, text/event-stream")
			}
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404:\n%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "route_not_found") {
				t.Errorf("body = %s, want route_not_found", rec.Body.String())
			}
		})
	}
}

// TestMCPRoute_NoServerFactory_503 covers the wiring gap the injected
// factory introduces: an embedder that mounts this package's routes without
// a Config.MCPServerFactory has no tool registry to serve. It is diagnosed
// BEFORE the address work — an unwired deployment cannot serve the route at
// any address — and named rather than nil-dereferenced or served as an empty
// tool list. The listener here is loopback, so the address ladder would
// otherwise have let the request through.
func TestMCPRoute_NoServerFactory_503(t *testing.T) {
	repo, token := mcpTestTokens(t)
	s := New(Config{Addr: "127.0.0.1:8080", APITokenRepo: repo})
	if s.mcpHandler != nil {
		t.Fatal("an unwired route built a handler")
	}
	req := newMCPPost(mcpInitializeBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "mcp_route_misconfigured") {
		t.Errorf("body = %s, want mcp_route_misconfigured", body)
	}
	if !strings.Contains(body, "no tool-server factory wired") {
		t.Errorf("body = %s, want the unwired-factory diagnosis", body)
	}
}

// TestMCPRoute_MalformedAddr_503 proves the malformed-Addr branch is
// REACHABLE: it is diagnosed ahead of the loopback predicate, which would
// otherwise swallow every unparseable address into a 403.
func TestMCPRoute_MalformedAddr_503(t *testing.T) {
	for _, addr := range []string{"nonsense", "::::", "127.0.0.1:80:90"} {
		t.Run(addr, func(t *testing.T) {
			s, token := mcpTestServer(t, Config{Addr: addr})
			req := newMCPPost(mcpInitializeBody)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "mcp_route_misconfigured") {
				t.Errorf("body = %s, want mcp_route_misconfigured", body)
			}
			if !strings.Contains(body, "is not a valid host:port") {
				t.Errorf("body = %s, want the unparseable-listen-address diagnosis", body)
			}
		})
	}
}

// TestMCPRoute_NonLoopbackListener_403 pins the ADR-033 refusal. The `:8080`
// row is the shipped FISHHAWKD_ADDR default and must never silently flip to
// loopback.
func TestMCPRoute_NonLoopbackListener_403(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080", "192.0.2.1:8080", "[::]:8080"} {
		t.Run(addr, func(t *testing.T) {
			s, token := mcpTestServer(t, Config{Addr: addr})
			req := newMCPPost(mcpInitializeBody)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403:\n%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "mcp_route_loopback_only") {
				t.Errorf("body = %s, want mcp_route_loopback_only", body)
			}
			for _, want := range []string{"ADR-033", "FISHHAWKD_ADDR=127.0.0.1:8080", "--mcp-route=off"} {
				if !strings.Contains(body, want) {
					t.Errorf("body = %s, want it to name %q", body, want)
				}
			}
		})
	}
}

// TestMCPRoute_NonLoopbackSelfURL_503 is the defect-2 refusal with a
// LOOPBACK listener, so the only thing that can produce the 503 is the self
// URL — the branch would be unreachable if the listener check absorbed it.
func TestMCPRoute_NonLoopbackSelfURL_503(t *testing.T) {
	s, token := mcpTestServer(t, Config{
		Addr:        "127.0.0.1:8080",
		MCPSelfURL:  "https://evil.example.com",
		mcpLookupIP: func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.5")}, nil },
	})
	req := newMCPPost(mcpInitializeBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "mcp_route_misconfigured") {
		t.Errorf("body = %s, want mcp_route_misconfigured", body)
	}
	if !strings.Contains(body, "off-host") {
		t.Errorf("body = %s, want the bearer-forwarding diagnosis", body)
	}
}

// TestMCPRoute_SelfURLLookupFailure_503 is the sibling degrade: a hostname
// self URL whose resolution FAILS refuses rather than being accepted
// unclassified.
func TestMCPRoute_SelfURLLookupFailure_503(t *testing.T) {
	s, token := mcpTestServer(t, Config{
		Addr:        "127.0.0.1:8080",
		MCPSelfURL:  "http://mcp.internal:8080",
		mcpLookupIP: func(string) ([]net.IP, error) { return nil, errors.New("resolver timeout") },
	})
	req := newMCPPost(mcpInitializeBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resolver timeout") {
		t.Errorf("body = %s, want the resolver diagnosis", rec.Body.String())
	}
}

// TestMCPRoute_ListenerLookupFailure_503 covers the same degrade one rung
// higher: the LISTENER host is a hostname that will not resolve. It is a 503
// diagnosis, not the 403 loopback refusal — the route cannot classify the
// address at all.
func TestMCPRoute_ListenerLookupFailure_503(t *testing.T) {
	s, token := mcpTestServer(t, Config{
		Addr:        "broken.internal:8080",
		mcpLookupIP: func(string) ([]net.IP, error) { return nil, errors.New("nxdomain") },
	})
	req := newMCPPost(mcpInitializeBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cannot classify its listen address") {
		t.Errorf("body = %s, want the classification diagnosis", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Auth gate
// ---------------------------------------------------------------------------

// mcpAuthEnvelope drives one /mcp POST and returns (status, body).
func mcpAuthEnvelope(t *testing.T, s *Server, decorate func(*http.Request)) (int, string) {
	t.Helper()
	req := newMCPPost(mcpInitializeBody)
	if decorate != nil {
		decorate(req)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestMCPRoute_AuthGate_401 covers the three credential shapes that must all
// be refused, and asserts the MISSING and UNKNOWN bearer cases return a
// BYTE-IDENTICAL envelope: discriminating them would leak token validity.
func TestMCPRoute_AuthGate_401(t *testing.T) {
	sessions, cookie := mcpTestSession(t)
	tokenRepo, _ := mcpTestTokens(t)
	s := New(Config{Addr: "127.0.0.1:8080", APITokenRepo: tokenRepo, AuthRepo: sessions,
		MCPServerFactory: testMCPServerFactory})

	missingStatus, missingBody := mcpAuthEnvelope(t, s, nil)
	unknownStatus, unknownBody := mcpAuthEnvelope(t, s, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer fhk_not_a_real_token")
	})
	cookieStatus, cookieBody := mcpAuthEnvelope(t, s, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
	})

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing bearer", missingStatus, missingBody},
		{"unknown bearer", unknownStatus, unknownBody},
		{"cookie session", cookieStatus, cookieBody},
	} {
		if tc.status != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401:\n%s", tc.name, tc.status, tc.body)
		}
		if !strings.Contains(tc.body, "authentication_required") {
			t.Errorf("%s: body = %s, want authentication_required", tc.name, tc.body)
		}
	}
	if missingBody != unknownBody {
		t.Errorf("missing-bearer and unknown-bearer envelopes differ, which leaks token validity:\nmissing: %s\nunknown: %s",
			missingBody, unknownBody)
	}
	if !strings.Contains(cookieBody, "cookie session is not accepted") {
		t.Errorf("cookie-session body = %s, want it to name the cookie refusal", cookieBody)
	}
}

// TestMCPRoute_CookieSessionReachesHandlerNotCSRF is the assertion that makes
// the csrfExemptPath entry load-bearing rather than decorative: a
// cookie-session POST with NO X-CSRF-Token would be rejected 403
// csrf_required WITHOUT the exemption. With it, the request reaches
// handleMCP, which refuses it 401 on its own terms — so the exemption can
// never become a browser-driven CSRF path onto the tool surface.
func TestMCPRoute_CookieSessionReachesHandlerNotCSRF(t *testing.T) {
	if !csrfExemptPath("/mcp") {
		t.Fatal("/mcp is not csrf-exempt; this test's premise is gone")
	}
	sessions, cookie := mcpTestSession(t)
	tokenRepo, _ := mcpTestTokens(t)
	s := New(Config{Addr: "127.0.0.1:8080", APITokenRepo: tokenRepo, AuthRepo: sessions,
		MCPServerFactory: testMCPServerFactory})
	status, body := mcpAuthEnvelope(t, s, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
	})
	if status == http.StatusForbidden || strings.Contains(body, "csrf_required") {
		t.Fatalf("cookie-session POST was rejected by CSRF (%d): %s", status, body)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from handleMCP's bearer gate:\n%s", status, body)
	}
}

// TestMCPRoute_BearerBypassesCSRF asserts a bearer POST and a bearer DELETE
// with no X-CSRF-Token are not rejected by the CSRF middleware.
func TestMCPRoute_BearerBypassesCSRF(t *testing.T) {
	s, token := mcpTestServer(t, Config{Addr: "127.0.0.1:8080"})

	post := newMCPPost(mcpInitializeBody)
	post.Header.Set("Authorization", "Bearer "+token)
	postRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(postRec, post)
	if postRec.Code == http.StatusForbidden || strings.Contains(postRec.Body.String(), "csrf_required") {
		t.Errorf("POST /mcp was CSRF-rejected (%d): %s", postRec.Code, postRec.Body.String())
	}

	del := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	del.Header.Set("Authorization", "Bearer "+token)
	del.Header.Set("Mcp-Session-Id", "fabricated-session")
	delRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(delRec, del)
	if delRec.Code == http.StatusForbidden || strings.Contains(delRec.Body.String(), "csrf_required") {
		t.Errorf("DELETE /mcp was CSRF-rejected (%d): %s", delRec.Code, delRec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Stateless transport consequences
// ---------------------------------------------------------------------------

// TestMCPRoute_GET_405WithAllow pins the honest cost of stateless mode: no
// standalone SSE stream, so GET answers the streamable-HTTP spec's
// prescribed 405 with an Allow header (RFC 9110 §15.5.6).
func TestMCPRoute_GET_405WithAllow(t *testing.T) {
	s, token := mcpTestServer(t, Config{Addr: "127.0.0.1:8080"})
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405:\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

// TestMCPRoute_DELETE_405WithAllow pins the other consequence, and the
// stronger form it took in go-sdk v1.7.0: a stateless server holds no
// session to tear down, and since v1.7.0 it does not read Mcp-Session-Id
// at all, so DELETE is refused outright rather than accepted as the 204
// no-op it answered through v1.6.1.
//
// The fabricated Mcp-Session-Id is the point of the test, not incidental
// setup: a 405 here proves the header bought the caller nothing. If this
// ever regresses to 204, session handling has been reintroduced into
// stateless mode (see the MCPGODEBUG note on newMCPHandler) and the
// per-request token binding this route's security argument rests on no
// longer holds.
func TestMCPRoute_DELETE_405WithAllow(t *testing.T) {
	s, token := mcpTestServer(t, Config{Addr: "127.0.0.1:8080"})
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "fabricated-session")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405:\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

// ---------------------------------------------------------------------------
// Registry parity and construction
// ---------------------------------------------------------------------------

// TestMCPRoute_ToolRegistryParity asserts SET equality between the tools the
// route's per-request server advertises over POST /mcp and the tools
// mcpserver.NewServer registers directly. A COUNT would pass a forked list of
// the same size; the set is what proves there is ONE registration path.
func TestMCPRoute_ToolRegistryParity(t *testing.T) {
	ctx := context.Background()
	ts := httptest.NewServer(nil)
	defer ts.Close()

	s, token := mcpTestServer(t, Config{Addr: ts.Listener.Addr().String()})
	ts.Config.Handler = s.Handler()

	overHTTP := mcpToolNamesOverRoute(t, ctx, ts.URL, token)
	direct := mcpToolNamesDirect(t, ctx)

	if len(direct) == 0 {
		t.Fatal("mcpserver.NewServer advertised zero tools; the comparison would be vacuous")
	}
	for name := range direct {
		if !overHTTP[name] {
			t.Errorf("tool %q is registered by mcpserver.NewServer but missing over POST /mcp", name)
		}
	}
	for name := range overHTTP {
		if !direct[name] {
			t.Errorf("tool %q is served over POST /mcp but is not in mcpserver.NewServer's registry", name)
		}
	}
}

// mcpToolNamesDirect lists the tools mcpserver.NewServer registers, over the
// SDK's in-memory transport (no HTTP involved).
func mcpToolNamesDirect(t *testing.T, ctx context.Context) map[string]bool {
	t.Helper()
	srv := mcpserver.NewServer(mcpserver.Config{BackendURL: "http://127.0.0.1:1", APIToken: "fhk_unused"})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "parity", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	names := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		names[tool.Name] = true
	}
	return names
}

// ---------------------------------------------------------------------------
// Construction-time resolution
// ---------------------------------------------------------------------------

// TestMCPRoute_LoopbackVerdictResolvedOnce proves the DNS lookup a hostname
// listen address needs happens at New() and NEVER on the request path: a
// resolver outage must not be able to intermittently flip a serving route to
// 403, and no caller should pay a blocking lookup.
func TestMCPRoute_LoopbackVerdictResolvedOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	lookup := func(string) ([]net.IP, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	s, token := mcpTestServer(t, Config{Addr: "localhost:8080", mcpLookupIP: lookup})

	mu.Lock()
	afterNew := calls
	mu.Unlock()
	if afterNew != 1 {
		t.Fatalf("lookups during New = %d, want exactly 1", afterNew)
	}

	for i := 0; i < 5; i++ {
		req := newMCPPost(mcpInitializeBody)
		req.Header.Set("Authorization", "Bearer "+token)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("lookups after 5 requests = %d, want still 1 (the verdict is resolved once at New)", calls)
	}
}

// TestMCPRoute_FactoryRefusesTokenlessRequest pins the getServer factory's
// own guard. It is unreachable through handleMCP (the 401 fires first), so
// it is asserted directly — the comment on it says the guard MUST NOT be
// simplified away, and this is what would fail if it were.
func TestMCPRoute_FactoryRefusesTokenlessRequest(t *testing.T) {
	h := newMCPHandler(mcpRouteState{mode: mcpRouteEnabled, selfURL: "http://127.0.0.1:1"}, testMCPServerFactory, nil)
	req := newMCPPost(mcpInitializeBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (the SDK's documented nil-server response):\n%s", rec.Code, rec.Body.String())
	}
}

// mcpRouteDeadline bounds the MCP client calls in these tests so a transport
// regression fails fast instead of hanging the package.
const mcpRouteDeadline = 60 * time.Second

// bearerRoundTripper attaches a fixed Authorization header to every request
// the MCP client transport makes. This is how a test binds a client session
// to a specific token — the property the stateless mode exists to make
// per-request.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// mcpClientSession connects an MCP client to baseURL+"/mcp", authenticating
// every request with token.
//
// DisableStandaloneSSE is REQUIRED against this route: the client would
// otherwise open the standalone GET stream, which a stateless server answers
// 405 by design. Treat a missing Mcp-Session-Id response header as expected
// stateless behaviour, not a failure.
func mcpClientSession(t *testing.T, ctx context.Context, baseURL, token string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             strings.TrimSuffix(baseURL, "/") + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRoundTripper{token: token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcproute-test", Version: "0"}, opts).
		Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect MCP client to %s: %v", transport.Endpoint, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// mcpToolNamesOverRoute lists the tools the route's per-request server
// advertises, driven as a real client over POST /mcp.
func mcpToolNamesOverRoute(t *testing.T, ctx context.Context, baseURL, token string) map[string]bool {
	t.Helper()
	cs := mcpClientSession(t, ctx, baseURL, token, nil)
	names := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools over /mcp: %v", err)
		}
		names[tool.Name] = true
	}
	return names
}
