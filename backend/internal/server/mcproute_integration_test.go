package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// inboundCall is one request observed arriving at fishhawkd's own listener.
// The MCP tools reach data by dialling that listener, so a tool call shows up
// here as a second, inner request — which is the only place the token a tool
// ACTUALLY used can be observed. A response body could be plausible while the
// binding was stale; the inbound header cannot.
type inboundCall struct {
	method string
	path   string
	host   string
	auth   string
}

// inboundRecorder wraps the real fishhawkd handler and records every /v0/
// request that reaches it.
type inboundRecorder struct {
	next http.Handler

	mu    sync.Mutex
	calls []inboundCall
}

func (rec *inboundRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v0/") {
		rec.mu.Lock()
		rec.calls = append(rec.calls, inboundCall{
			method: r.Method,
			path:   r.URL.Path,
			host:   r.Host,
			auth:   r.Header.Get("Authorization"),
		})
		rec.mu.Unlock()
	}
	rec.next.ServeHTTP(w, r)
}

func (rec *inboundRecorder) snapshot() []inboundCall {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]inboundCall(nil), rec.calls...)
}

func (rec *inboundRecorder) reset() {
	rec.mu.Lock()
	rec.calls = nil
	rec.mu.Unlock()
}

// mcpFixture is a real fishhawkd serving on a real loopback listener, with
// its /mcp self URL pointed at that same listener.
type mcpFixture struct {
	ts       *httptest.Server
	server   *Server
	runs     *fakeRepo
	tokens   *fakeTokenRepo
	recorder *inboundRecorder
}

// newMCPFixture stands up the cross-layer harness.
//
// httptest.NewUnstartedServer allocates its listener at CONSTRUCTION and only
// begins serving on Start, so the listen address — and therefore the self URL
// the tools dial back on — is known BEFORE server.New builds the route state.
// Without that ordering the loopback round-trip would have no reachable base
// URL and every case here would fail outright.
func newMCPFixture(t *testing.T, mutate func(*Config)) *mcpFixture {
	t.Helper()

	ts := httptest.NewUnstartedServer(nil)
	t.Cleanup(ts.Close)
	addr := ts.Listener.Addr().String()

	runs := newFakeRepo()
	tokens := newFakeTokenRepo()
	cfg := Config{
		Addr:             addr,
		MCPSelfURL:       "http://" + addr,
		RunRepo:          runs,
		APITokenRepo:     tokens,
		AuditRepo:        newAuditFake(),
		AccountRoles:     fakeAccountRoles{role: account.RoleAdmin},
		MCPServerFactory: testMCPServerFactory,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s := New(cfg)
	if s.mcpRoute.mode != mcpRouteEnabled {
		t.Fatalf("route mode = %v (reason %q), want mcpRouteEnabled on a loopback httptest listener",
			s.mcpRoute.mode, s.mcpRoute.reason)
	}

	rec := &inboundRecorder{next: s.Handler()}
	ts.Config.Handler = rec
	ts.Start()

	return &mcpFixture{ts: ts, server: s, runs: runs, tokens: tokens, recorder: rec}
}

// issueToken mints a live bearer on the fixture's token repo.
func (f *mcpFixture) issueToken(t *testing.T, subject string, scopes ...string) string {
	t.Helper()
	tok, err := f.tokens.Issue(context.Background(), subject, scopes)
	if err != nil {
		t.Fatalf("issue token %s: %v", subject, err)
	}
	return tok.PlainText
}

// seedRun puts one run in the repo and returns it.
func (f *mcpFixture) seedRun(t *testing.T) *run.Run {
	t.Helper()
	r, err := f.runs.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return r
}

// postMCP sends one raw streamable-HTTP POST to /mcp bearing token, with a
// FABRICATED Mcp-Session-Id. Stateless mode ignores the header entirely; a
// raw POST (rather than an SDK client session) is what lets a test control it.
func (f *mcpFixture) postMCP(t *testing.T, token, sessionID, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /mcp response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// callToolBody renders a tools/call JSON-RPC request.
func callToolBody(id int, name string, args map[string]any) string {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		panic(err)
	}
	return string(payload)
}

// lastInboundFor returns the most recent recorded inner call to path.
func lastInboundFor(t *testing.T, calls []inboundCall, path string) inboundCall {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].path == path {
			return calls[i]
		}
	}
	t.Fatalf("no inbound call to %s was recorded; observed %+v", path, calls)
	return inboundCall{}
}

// ---------------------------------------------------------------------------
// (a) The defect-1 regression pin
// ---------------------------------------------------------------------------

// TestMCPRoute_PerRequestBearerBinding is THE DEFECT-1 REGRESSION PIN.
//
// The property: the token a tool call authenticates with is the token on the
// HTTP request that triggered that call — never a token captured earlier and
// held by a reused session. It is what makes it safe to mount a
// multi-credential tool surface on a shared listener.
//
// Under stateless semantics there is no reusable session to capture and
// replay, so the procedure is: issue two requests bearing DIFFERENT bearers,
// both sending the SAME fabricated Mcp-Session-Id (which stateless ignores),
// and assert the downstream loopback call for the second request arrives
// carrying the SECOND token — observed on the recorded inbound Authorization
// header, never inferred from the response body, since a stale binding can
// still return a plausible answer.
//
// A missing Mcp-Session-Id response header is EXPECTED stateless behaviour,
// not a failure.
//
// This fails loudly if StreamableHTTPOptions.Stateless is ever removed from
// newMCPHandler, or if the SDK's getServer contract changes so the factory
// stops running per request.
func TestMCPRoute_PerRequestBearerBinding(t *testing.T) {
	f := newMCPFixture(t, nil)
	tokenA := f.issueToken(t, "svc:alpha", "read:runs")
	tokenB := f.issueToken(t, "svc:bravo", "read:runs")
	if tokenA == tokenB {
		t.Fatal("the two bearers are identical; the test would be vacuous")
	}
	f.seedRun(t)

	const fabricatedSession = "fabricated-session-id-that-stateless-ignores"
	body := callToolBody(1, "fishhawk_list_runs", map[string]any{})

	statusA, respA := f.postMCP(t, tokenA, fabricatedSession, body)
	if statusA != http.StatusOK {
		t.Fatalf("first call status = %d, want 200:\n%s", statusA, respA)
	}
	firstInbound := lastInboundFor(t, f.recorder.snapshot(), "/v0/runs")
	if firstInbound.auth != "Bearer "+tokenA {
		t.Fatalf("first downstream call carried %q, want the FIRST token", firstInbound.auth)
	}

	f.recorder.reset()
	statusB, respB := f.postMCP(t, tokenB, fabricatedSession, body)
	if statusB != http.StatusOK {
		t.Fatalf("second call status = %d, want 200:\n%s", statusB, respB)
	}
	secondInbound := lastInboundFor(t, f.recorder.snapshot(), "/v0/runs")
	if secondInbound.auth == "Bearer "+tokenA {
		t.Fatal("the second request's tool call reached the backend carrying the FIRST request's token: " +
			"the tool server is bound per session, not per request (defect 1 has regressed)")
	}
	if secondInbound.auth != "Bearer "+tokenB {
		t.Fatalf("second downstream call carried %q, want the SECOND token", secondInbound.auth)
	}
}

// TestMCPRoute_SingleTokenControl is the control for the pin above: with one
// token throughout, the same raw-POST procedure still works. Without it, a
// broken transport that never reached the backend at all would make the
// binding assertion vacuously pass.
func TestMCPRoute_SingleTokenControl(t *testing.T) {
	f := newMCPFixture(t, nil)
	token := f.issueToken(t, "svc:solo", "read:runs")
	seeded := f.seedRun(t)

	status, resp := f.postMCP(t, token, "", callToolBody(1, "fishhawk_list_runs", map[string]any{}))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", status, resp)
	}
	if !strings.Contains(resp, seeded.ID.String()) {
		t.Fatalf("tool result does not mention the seeded run %s:\n%s", seeded.ID, resp)
	}
	inbound := lastInboundFor(t, f.recorder.snapshot(), "/v0/runs")
	if inbound.auth != "Bearer "+token {
		t.Errorf("downstream call carried %q, want %q", inbound.auth, "Bearer "+token)
	}
}

// ---------------------------------------------------------------------------
// (b) End-to-end parity
// ---------------------------------------------------------------------------

// TestMCPRoute_ToolAnswerMatchesRESTAnswer is the seam no per-layer unit can
// cover: the tools reach data by dialling fishhawkd's OWN listener, so the
// route, the middleware chain, the self-URL derivation and the tool library
// must all be correct simultaneously for the answer to come back at all.
func TestMCPRoute_ToolAnswerMatchesRESTAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpRouteDeadline)
	defer cancel()

	f := newMCPFixture(t, nil)
	token := f.issueToken(t, "svc:parity", "read:runs")
	seeded := f.seedRun(t)

	// Through the MCP route, driven as a real client session.
	cs := mcpClientSession(t, ctx, f.ts.URL, token, nil)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_list_runs", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool over /mcp: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	var toolOut struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(structured, &toolOut); err != nil {
		t.Fatalf("decode tool output %s: %v", structured, err)
	}

	// The same question over REST, with the SAME token.
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+"/v0/runs", nil)
	if err != nil {
		t.Fatalf("build REST request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v0/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v0/runs status = %d, want 200", resp.StatusCode)
	}
	var restOut struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&restOut); err != nil {
		t.Fatalf("decode REST response: %v", err)
	}

	var toolIDs, restIDs []string
	for _, it := range toolOut.Items {
		toolIDs = append(toolIDs, it.ID)
	}
	for _, it := range restOut.Items {
		restIDs = append(restIDs, it.ID)
	}
	if len(toolIDs) == 0 {
		t.Fatal("the MCP tool returned zero runs; the parity comparison would be vacuous")
	}
	if !slices.Equal(toolIDs, restIDs) {
		t.Errorf("MCP tool run ids %v != REST run ids %v", toolIDs, restIDs)
	}
	if len(toolIDs) != 1 || toolIDs[0] != seeded.ID.String() {
		t.Errorf("run ids = %v, want exactly the seeded run %s", toolIDs, seeded.ID)
	}
}

// ---------------------------------------------------------------------------
// (c) Denial parity
// ---------------------------------------------------------------------------

// TestMCPRoute_DenialParity asserts the route grants no privilege the REST
// surface would not: a token that GET/POST would refuse is refused
// identically through a tool call, because the tool authenticates by dialling
// the same REST route with the same bearer.
func TestMCPRoute_DenialParity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpRouteDeadline)
	defer cancel()

	f := newMCPFixture(t, nil)
	// read-only: enough to reach the tool surface, not enough to cancel.
	readOnly := f.issueToken(t, "svc:readonly", "read:runs")
	seeded := f.seedRun(t)

	// REST refuses.
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+"/v0/runs/"+seeded.ID.String()+"/cancel", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build REST request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+readOnly)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	restBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("REST cancel status = %d, want 403:\n%s", resp.StatusCode, restBody)
	}
	if !strings.Contains(string(restBody), "insufficient_scope") {
		t.Fatalf("REST cancel body = %s, want insufficient_scope", restBody)
	}

	// The MCP route refuses identically.
	cs := mcpClientSession(t, ctx, f.ts.URL, readOnly, nil)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_cancel_run", Arguments: map[string]any{"run_id": seeded.ID.String()},
	})
	if err == nil && !res.IsError {
		t.Fatal("fishhawk_cancel_run SUCCEEDED through /mcp with a token REST refuses: " +
			"the route granted privilege the REST surface does not")
	}
	denial := toolFailureText(res, err)
	if !strings.Contains(denial, "403") || !strings.Contains(denial, "insufficient_scope") {
		t.Errorf("tool denial = %q, want the same 403 insufficient_scope REST returned", denial)
	}
}

// toolFailureText renders whichever failure channel a tool call used — a
// transport error or an IsError result — as one comparable string.
func toolFailureText(res *mcp.CallToolResult, err error) string {
	if err != nil {
		return err.Error()
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// (d) In-request progress notifications (CONDITION 2)
// ---------------------------------------------------------------------------

// TestMCPRoute_InRequestProgressNotification is the CONDITION-2 demonstration.
//
// Dropping the standalone SSE stream is only acceptable if IN-REQUEST
// notifications survive: Fishhawk's long-running tools (run_stage output,
// await_review/await_audit, the #1963 heartbeat) depend on them, and they
// ride the POST response's own stream rather than the standalone GET stream.
// The go-sdk documents this for stateless mode ("Server->Client notifications
// may reach the client if they are made in the context of an incoming
// request"), but a doc comment is not a demonstration — so this drives a real
// long-running tool over POST /mcp with a progressToken and asserts at least
// one progress notification reaches the client on that response.
//
// fishhawk_await_audit is the vehicle because it emits one heartbeat per poll
// tick. The tick interval is the tool's own 3s default and is not injectable
// from this package, so the call is armed with a timeout comfortably past one
// tick and the test costs a few seconds of wall clock. That is the price of
// proving the property at the real HTTP boundary rather than in-memory.
func TestMCPRoute_InRequestProgressNotification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpRouteDeadline)
	defer cancel()

	f := newMCPFixture(t, nil)
	token := f.issueToken(t, "svc:progress", "read:runs")
	seeded := f.seedRun(t)

	var mu sync.Mutex
	var notes []*mcp.ProgressNotificationParams
	cs := mcpClientSession(t, ctx, f.ts.URL, token, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			notes = append(notes, req.Params)
			mu.Unlock()
		},
	})

	params := &mcp.CallToolParams{
		Name: "fishhawk_await_audit",
		Arguments: map[string]any{
			"run_id":          seeded.ID.String(),
			"category":        "implement_reviewed",
			"timeout_seconds": 5,
		},
	}
	params.SetProgressToken("mcproute-progress-1")

	start := time.Now()
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("the await returned after %s, before the tool's first 3s poll tick — "+
			"no heartbeat could have been emitted, so this run proves nothing", elapsed)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(notes) == 0 {
		t.Fatal("ZERO progress notifications reached the client over POST /mcp. Stateless mode does not " +
			"deliver in-request notifications, which invalidates the transport-mode decision — STOP and " +
			"re-plan rather than relaxing this assertion")
	}
	for i, n := range notes {
		if n.ProgressToken != "mcproute-progress-1" {
			t.Errorf("notification[%d] progressToken = %v, want the request's token", i, n.ProgressToken)
		}
	}
}

// ---------------------------------------------------------------------------
// (e) CONDITION 3 end-to-end: the dialled URL carries the pinned IP
// ---------------------------------------------------------------------------

// TestMCPRoute_SelfURLPinnedToResolvedIP is the CONDITION-3 end-to-end proof.
//
// The self URL is configured as a HOSTNAME ("localhost:<port>") whose
// injected resolver answers loopback at construction and a PUBLIC address on
// every lookup afterwards. If the route stored the hostname, the HTTP client
// would re-resolve it at call time. The assertion is on the Host header the
// loopback call ACTUALLY arrives with: a pinned IP literal proves the
// hostname was resolved once and frozen, because the two are otherwise
// indistinguishable on this machine (localhost really does resolve to
// 127.0.0.1, so the call would still land — just with the wrong Host, and in
// production against the wrong host entirely).
func TestMCPRoute_SelfURLPinnedToResolvedIP(t *testing.T) {
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

	var listenPort string
	f := newMCPFixture(t, func(cfg *Config) {
		_, port, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			t.Fatalf("split listener addr %q: %v", cfg.Addr, err)
		}
		listenPort = port
		cfg.MCPSelfURL = "http://localhost:" + port
		cfg.mcpLookupIP = lookup
	})

	wantSelf := "http://127.0.0.1:" + listenPort
	if f.server.mcpRoute.selfURL != wantSelf {
		t.Fatalf("selfURL = %q, want the pinned %q", f.server.mcpRoute.selfURL, wantSelf)
	}

	token := f.issueToken(t, "svc:pinned", "read:runs")
	f.seedRun(t)
	status, resp := f.postMCP(t, token, "", callToolBody(1, "fishhawk_list_runs", map[string]any{}))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", status, resp)
	}

	inbound := lastInboundFor(t, f.recorder.snapshot(), "/v0/runs")
	if inbound.host != "127.0.0.1:"+listenPort {
		t.Fatalf("the loopback call arrived with Host %q, want the pinned %q — the self URL kept the "+
			"hostname, so the HTTP client re-resolved it at call time and a DNS change would carry the "+
			"caller's bearer off-host", inbound.host, "127.0.0.1:"+listenPort)
	}

	// Prove the resolver really did flip, so the assertion above is about
	// pinning rather than a record that never changed.
	after, err := lookup("localhost")
	if err != nil || len(after) != 1 || after[0].IsLoopback() {
		t.Fatalf("post-construction lookup = %v (err %v), want a single non-loopback address", after, err)
	}
}

// ---------------------------------------------------------------------------
// (e) OAuth AS-issued (fho_) access tokens across all four layers (#2391)
// ---------------------------------------------------------------------------

// newOAuthMCPFixture stands up the cross-layer harness with the OAuth AS enabled
// over store (issuer https://as.example, resource https://as.example/mcp).
func newOAuthMCPFixture(t *testing.T, store *fakeOAuthStore) *mcpFixture {
	t.Helper()
	return newMCPFixture(t, func(cfg *Config) {
		cfg.OAuthASIssuer = testIssuer
		cfg.OAuthStore = store
		cfg.OAuthCIMDFetcher = newCIMDFetcher(newCIMD())
	})
}

// TestMCPRoute_OAuthTokenEndToEnd is the cross-layer proof (HTTP handler +
// middleware + oauthas domain + oauthstore persistence): a matching-audience
// fho_ access token presented at POST /mcp is accepted, drives a real tool call,
// and the tool's dial-back to fishhawkd's own REST API carries that same fho_
// token — which fails on a 401 unless the fho_ token authenticates on ordinary
// REST routes too (the assumption that drove audience validation into
// bearerAuth). Seeded through the store, never through the control under test.
func TestMCPRoute_OAuthTokenEndToEnd(t *testing.T) {
	store := newFakeOAuthStore()
	f := newOAuthMCPFixture(t, store)
	const fho = "fho_endtoendendtoendendtoendendtoendend01"
	store.seedAccessToken(fho, "github:octocat", testResource, "", []string{"read:runs"})
	seeded := f.seedRun(t)

	status, resp := f.postMCP(t, fho, "", callToolBody(1, "fishhawk_list_runs", map[string]any{}))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", status, resp)
	}
	if !strings.Contains(resp, seeded.ID.String()) {
		t.Fatalf("tool result does not mention the seeded run %s:\n%s", seeded.ID, resp)
	}
	inbound := lastInboundFor(t, f.recorder.snapshot(), "/v0/runs")
	if inbound.auth != "Bearer "+fho {
		t.Errorf("dial-back carried %q, want the fho_ token %q", inbound.auth, "Bearer "+fho)
	}
}

// TestMCPRoute_ForeignAudienceTokenRejectedEndToEnd seeds a token whose audience
// is a DIFFERENT resource — bad state BY CONSTRUCTION, never by calling the
// control — and asserts /mcp answers the 401 discovery challenge, never admitting
// it to the tool surface.
func TestMCPRoute_ForeignAudienceTokenRejectedEndToEnd(t *testing.T) {
	store := newFakeOAuthStore()
	f := newOAuthMCPFixture(t, store)
	const fho = "fho_foreignforeignforeignforeignforeign01"
	store.seedAccessToken(fho, "github:mallory", "https://other.example/mcp", "", []string{"read:runs"})
	f.seedRun(t)

	status, resp := f.postMCP(t, fho, "", callToolBody(1, "fishhawk_list_runs", map[string]any{}))
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (foreign audience rejected):\n%s", status, resp)
	}
	if !strings.Contains(resp, "authentication_required") {
		t.Errorf("body = %s, want authentication_required", resp)
	}
	// The foreign-audience token must never have reached the tool dial-back.
	for _, c := range f.recorder.snapshot() {
		if c.path == "/v0/runs" {
			t.Errorf("foreign-audience token reached the REST dial-back at %s; it must be refused at /mcp", c.path)
		}
	}
}
