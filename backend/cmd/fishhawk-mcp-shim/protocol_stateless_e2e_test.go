package main

// End-to-end proof of the two-protocol swap contract (#2460) with a REAL go-sdk
// v1.7.0 mcp.Client driven through the supervisor against REAL mcp.Server
// children. The frame-level suite in supervisor_test.go pins each supervisor
// branch against synthetic frames; this file pins what the CLIENT observes —
// its ListTools result, its list-changed handler, the subscription id stamped on
// notifications after a swap — so a change that keeps every synthetic frame
// byte-identical but breaks the protocol as the SDK actually speaks it still
// fails a named test.
//
// The file is self-contained: it defines its own child transport and client
// plumbing and hoists nothing out of the shared harness in supervisor_test.go
// (approval condition 3). It reuses only that file's mutex-guarded sinks
// (pubLog, syncBuf), which are package-level types, not harness helpers.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// e2eWait bounds every await in this file. The supervisor swap is synchronous
// once triggered and the SDK's list-changed debounce is 10ms, so a bound this
// generous only matters when something is genuinely broken.
const e2eWait = 5 * time.Second

// --- child side: a real mcp.Server behind the childTransport seam ---

// e2eChild is a childTransport whose "process" is a real go-sdk mcp.Server
// running over io.Pipe halves. The supervisor drives it through the same seam
// as the os/exec stdioChild and never learns there is no process.
type e2eChild struct {
	name   string
	server *mcp.Server

	frames chan []byte
	exited chan error

	mu      sync.Mutex
	stdin   *io.PipeWriter // shim -> server
	cancel  context.CancelFunc
	sent    [][]byte // every frame the shim sent, byte-verbatim
	stopped bool
}

func newE2EChild(name string, tools ...string) *e2eChild {
	srv := mcp.NewServer(&mcp.Implementation{Name: "e2e-" + name, Version: "0"}, nil)
	srv.AddReceivingMiddleware(cacheableToolList)
	for _, tool := range tools {
		addE2ETool(srv, tool)
	}
	return &e2eChild{
		name:   name,
		server: srv,
		frames: make(chan []byte, 256),
		exited: make(chan error, 1),
	}
}

// e2eToolListTTL is the SEP-2549 ttlMs the e2e children stamp on every
// tools/list result. The go-sdk server ships ttlMs 0 (immediately stale), under
// which the client re-reads the list on EVERY ListTools and a missing
// list_changed is invisible to a content assertion. With a real TTL the client
// holds the list until a list_changed invalidates it, so "beta after the swap"
// is observable ONLY if the notification reached the client — which is what
// makes deleting the emission redden that assertion (approval condition 1).
const e2eToolListTTL = 60_000

// cacheableToolList is server middleware stamping e2eToolListTTL on every
// tools/list result.
func cacheableToolList(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if lt, ok := res.(*mcp.ListToolsResult); ok && err == nil {
			lt.TTLMs = e2eToolListTTL
		}
		return res, err
	}
}

// addE2ETool registers a no-op tool. The tool NAME is the whole fixture: two
// children registering different names are definitionally distinguishable from
// the client's ListTools, so the swap assertions land on behaviour, not setup.
func addE2ETool(srv *mcp.Server, name string) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "e2e tool " + name},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, nil, nil
		})
}

func (c *e2eChild) Start(ctx context.Context) error {
	inR, inW := io.Pipe()   // shim writes, server reads
	outR, outW := io.Pipe() // server writes, shim reads
	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.stdin = inW
	c.cancel = cancel
	c.mu.Unlock()
	go func() {
		err := c.server.Run(runCtx, &mcp.IOTransport{Reader: inR, Writer: outW})
		_ = outW.Close()
		c.exited <- err
	}()
	go func() {
		r := bufio.NewReader(outR)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				c.frames <- cloneBytes(line)
			}
			if err != nil {
				return
			}
		}
	}()
	return nil
}

func (c *e2eChild) Send(frame []byte) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return errors.New("send to terminated child " + c.name)
	}
	c.sent = append(c.sent, cloneBytes(frame))
	w := c.stdin
	c.mu.Unlock()
	// Written outside the lock: a pipe write completes only once the server's
	// reader consumes it, and sentFrames must never wait on that.
	_, err := w.Write(frame)
	return err
}

func (c *e2eChild) Frames() <-chan []byte { return c.frames }
func (c *e2eChild) Exited() <-chan error  { return c.exited }
func (c *e2eChild) LaunchHash() []byte    { return []byte("hash-" + c.name) }
func (c *e2eChild) Pid() int              { return 0 }

// Terminate closes the server's stdin (EOF ends the session, so Run returns and
// Exited fires) and cancels its context so a blocked subscriptions/listen
// handler unwinds. Idempotent.
func (c *e2eChild) Terminate(time.Duration) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	stdin, cancel := c.stdin, c.cancel
	c.mu.Unlock()
	_ = stdin.Close()
	cancel()
}

func (c *e2eChild) sentFrames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.sent))
	for i, b := range c.sent {
		out[i] = string(b)
	}
	return out
}

// sentMethods lists the JSON-RPC methods of every frame the shim sent this
// child, in order.
func (c *e2eChild) sentMethods() []string {
	var out []string
	for _, f := range c.sentFrames() {
		if p := peek([]byte(f)); p.hasMethod() {
			out = append(out, p.method())
		}
	}
	return out
}

// --- client side: a real mcp.Client over IOTransport into the supervisor ---

// e2eSession wires a supervisor between a real go-sdk client and the e2e
// children. The client's stdout pipe is pumped line-by-line into the
// supervisor's clientIn channel (the stdin reader main.go runs); the
// supervisor's clientOut is the client's stdin pipe.
type e2eSession struct {
	t   *testing.T
	sup *supervisor

	swap chan []byte
	tick chan time.Time
	pub  *pubLog
	logs *syncBuf

	transport    mcp.Transport  // the client's side
	clientWriter *io.PipeWriter // the client's stdout half; closed on cleanup
	toClient     *io.PipeWriter

	// listChanged receives every tools/list_changed the CLIENT's handler saw.
	listChanged chan *mcp.ToolListChangedRequest

	mu       sync.Mutex
	upstream [][]byte // every frame the client put on the wire, byte-verbatim

	done chan struct{} // closed when supervisor.run returns
}

// newE2ESession builds the session but does not start the supervisor loop.
func newE2ESession(t *testing.T, child0 childTransport, rest ...childTransport) *e2eSession {
	t.Helper()
	clientOutR, clientOutW := io.Pipe() // client writes, pump reads
	clientInR, clientInW := io.Pipe()   // supervisor writes, client reads

	in := make(chan []byte)
	idx := 0
	factory := func() childTransport {
		if idx >= len(rest) {
			panic("e2e child factory exhausted")
		}
		c := rest[idx]
		idx++
		return c
	}
	logs := &syncBuf{}
	pub := &pubLog{}
	sup := newSupervisor(child0, factory, nil, in, clientInW, logs, 30*time.Second, time.Second)
	swap := make(chan []byte)
	tick := make(chan time.Time)
	sup.swapReq = swap
	sup.tick = tick
	sup.publish = pub.add

	s := &e2eSession{
		t:            t,
		sup:          sup,
		swap:         swap,
		tick:         tick,
		pub:          pub,
		logs:         logs,
		transport:    &mcp.IOTransport{Reader: clientInR, Writer: clientOutW},
		clientWriter: clientOutW,
		toClient:     clientInW,
		listChanged:  make(chan *mcp.ToolListChangedRequest, 16),
		done:         make(chan struct{}),
	}
	// The stdin pump: one frame per line, in order, closing clientIn on EOF so
	// the supervisor terminates its child and returns — the real shutdown path.
	go func() {
		r := bufio.NewReader(clientOutR)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				s.mu.Lock()
				s.upstream = append(s.upstream, cloneBytes(line))
				s.mu.Unlock()
				in <- cloneBytes(line)
			}
			if err != nil {
				close(in)
				return
			}
		}
	}()
	return s
}

// start runs the supervisor loop and registers cleanup: close the client's
// writer (EOF to the pump, which closes clientIn), wait for run to return, then
// close the remaining pipe so no goroutine is left blocked.
func (s *e2eSession) start() {
	s.t.Helper()
	go func() {
		_ = s.sup.run(context.Background())
		close(s.done)
	}()
	s.t.Cleanup(func() {
		_ = s.clientWriter.Close()
		select {
		case <-s.done:
		case <-time.After(e2eWait):
			s.t.Errorf("supervisor did not return after client EOF; logs:\n%s", s.logs.String())
		}
		_ = s.toClient.Close()
	})
}

// connect opens a real go-sdk client session through the supervisor. The
// ToolListChangedHandler is what makes a SEP-2575 client open the
// subscriptions/listen stream at connect time.
func (s *e2eSession) connect(ctx context.Context) *mcp.ClientSession {
	s.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(_ context.Context, req *mcp.ToolListChangedRequest) {
			s.listChanged <- req
		},
	})
	cs, err := client.Connect(ctx, s.transport, nil)
	if err != nil {
		s.t.Fatalf("client connect through the shim: %v; logs:\n%s", err, s.logs.String())
	}
	s.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// upstreamMethods lists the methods of every request/notification the client
// put on the wire so far, in order.
func (s *e2eSession) upstreamMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, f := range s.upstream {
		if p := peek(f); p.hasMethod() {
			out = append(out, p.method())
		}
	}
	return out
}

// upstreamMetaVersion returns the protocol version the FIRST upstream request
// stamped into its params._meta — what the shim classified the session from.
func (s *e2eSession) upstreamMetaVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.upstream {
		if v := peek(f).metaProtocolVersion(); v != "" {
			return v
		}
	}
	return ""
}

// triggerSwap injects a content-change swap and waits for the supervisor to
// record it as swapped.
func (s *e2eSession) triggerSwap(hash string) swapState {
	s.t.Helper()
	s.swap <- []byte(hash)
	return s.pub.waitFor(s.t, "last_swap_outcome swapped", func(st swapState) bool {
		return st.LastSwapOutcome == outcomeSwapped
	})
}

// publishNow fires one watcher tick, which publishes a fresh snapshot without
// changing any state, and returns it.
func (s *e2eSession) publishNow() swapState {
	s.t.Helper()
	all := func(swapState) bool { return true }
	before := s.pub.count(all)
	s.tick <- time.Time{}
	deadline := time.Now().Add(e2eWait)
	for time.Now().Before(deadline) {
		if s.pub.count(all) > before {
			s.pub.mu.Lock()
			defer s.pub.mu.Unlock()
			return s.pub.states[len(s.pub.states)-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.t.Fatal("timed out waiting for the tick-published snapshot")
	return swapState{}
}

// awaitListChanged waits for the client's handler to fire and returns the
// request, or nil on timeout. It does NOT fail the test: the callers assert on
// the tool list the client then reads, so a missing notification lands on the
// end-to-end refresh assertion rather than on a timeout (approval condition 1).
func (s *e2eSession) awaitListChanged() *mcp.ToolListChangedRequest {
	select {
	case req := <-s.listChanged:
		return req
	case <-time.After(e2eWait):
		return nil
	}
}

// toolNames reads the client's current tool list.
func toolNames(ctx context.Context, t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// subscriptionIDOf renders the subscription id a notification carries in its
// _meta as its raw JSON token, the same encoding the shim's listen_stream_id
// uses, or "" when the notification is untagged.
func subscriptionIDOf(req *mcp.ToolListChangedRequest) string {
	if req == nil || req.Params == nil {
		return ""
	}
	v, ok := req.Params.GetMeta()[mcp.MetaKeySubscriptionID]
	if !ok {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// TestStatelessClientToolListRefreshesAfterSwap is the issue's AC1 end to end:
// a real 2026-07-28 client connected through the shim sees the NEW child's
// tool set after a swap, with no initialize ever crossing the wire, and its
// subscriptions/listen stream survives the swap — a tool added to the fresh
// child afterwards is announced under the ORIGINAL subscription id.
//
// Two counterfactuals discriminate the two controls this pins (approval
// condition 1): deleting the post-swap list_changed emission leaves the
// client's tools cache stale, so the "beta after swap" assertion goes red;
// deleting the listen re-send leaves the fresh child with no subscriber, so the
// "gamma announced" continuity assertion goes red.
func TestStatelessClientToolListRefreshesAfterSwap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	childA := newE2EChild("A", "alpha")
	childB := newE2EChild("B", "beta")
	s := newE2ESession(t, childA, childB)
	s.start()
	cs := s.connect(ctx)

	if got := cs.InitializeResult().ProtocolVersion; !isStatelessVersion(got) {
		t.Fatalf("negotiated protocol %q is not a stateless version", got)
	}
	if got := toolNames(ctx, t, cs); strings.Join(got, ",") != "alpha" {
		t.Fatalf("tools before swap = %v, want [alpha]", got)
	}
	for _, m := range s.upstreamMethods() {
		if m == "initialize" {
			t.Fatalf("a stateless client sent initialize; upstream methods %v", s.upstreamMethods())
		}
	}
	// The listen stream is on the wire ahead of the tools/list the client just
	// awaited, so the shim has recorded it: assert from a fresh snapshot.
	pre := s.publishNow()
	if pre.SessionProtocol != sessionProtocolStateless || pre.ListenStreamID == "" {
		t.Fatalf("pre-swap snapshot session_protocol=%q listen_stream_id=%q, want stateless with a recorded stream", pre.SessionProtocol, pre.ListenStreamID)
	}
	listenID := pre.ListenStreamID

	post := s.triggerSwap("hash-B")
	if post.SessionProtocol != sessionProtocolStateless || post.ListenStreamID != listenID {
		t.Fatalf("post-swap snapshot session_protocol=%q listen_stream_id=%q, want stateless/%s", post.SessionProtocol, post.ListenStreamID, listenID)
	}

	// AC1: the client re-reads the tool set from the NEW child. The SDK caches
	// ListTools under the new protocol, so only a delivered list_changed makes
	// this observe beta — a missing notification leaves alpha cached.
	if req := s.awaitListChanged(); req == nil {
		t.Logf("no tools/list_changed reached the client after the swap; reading the tool list anyway")
	}
	if got := toolNames(ctx, t, cs); strings.Join(got, ",") != "beta" {
		t.Fatalf("tools after swap = %v, want [beta] (client still on the old child or its cache was never invalidated)", got)
	}

	// Subscription continuity: the fresh child must hold the client's listen
	// stream under its ORIGINAL id. The child stamps that id into every
	// notification it routes, so a tool added to it now is announced to the
	// client tagged with the id the shim recorded before the swap. Without the
	// re-send the fresh child has no subscriber, nothing is announced, and the
	// client keeps serving its cached [beta].
	addE2ETool(childB.server, "gamma")
	req := s.awaitListChanged()
	if got := toolNames(ctx, t, cs); strings.Join(got, ",") != "beta,gamma" {
		t.Fatalf("tools after adding gamma to the fresh child = %v, want [beta gamma] (the listen stream was not re-established on the fresh child)", got)
	}
	if got := subscriptionIDOf(req); got != listenID {
		t.Fatalf("gamma's list_changed carried subscription id %q, want the original stream id %q", got, listenID)
	}
	if methods := childB.sentMethods(); len(methods) == 0 || methods[0] != methodSubscriptionsListen {
		t.Fatalf("fresh child's first frame was %v, want %s re-sent ahead of any client traffic", methods, methodSubscriptionsListen)
	}
}

// TestNegotiatedVersionMatchesShimConstant is AC3: the version a real go-sdk
// client negotiates over the shim — and stamps into every request's _meta,
// which is what the shim classifies from — equals statelessProtocolVersion.
// A go-sdk bump that moves latestProtocolVersion, or a shim edit that moves the
// constant, reddens this test by name rather than silently reclassifying
// sessions.
func TestNegotiatedVersionMatchesShimConstant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := newE2ESession(t, newE2EChild("A", "alpha"), newE2EChild("B", "beta"))
	s.start()
	cs := s.connect(ctx)

	if got := cs.InitializeResult().ProtocolVersion; got != statelessProtocolVersion {
		t.Fatalf("negotiated protocol %q != shim's statelessProtocolVersion %q — update the constant and the README two-protocol contract in lockstep", got, statelessProtocolVersion)
	}
	if got := s.upstreamMetaVersion(); got != statelessProtocolVersion {
		t.Fatalf("first request's _meta protocolVersion %q != %q", got, statelessProtocolVersion)
	}
	// The wire version is what the shim classified from: prove it published
	// the classification, not merely that the client reports the version.
	if st := s.publishNow(); st.SessionProtocol != sessionProtocolStateless {
		t.Fatalf("shim classified the session %q for wire version %q, want %q", st.SessionProtocol, statelessProtocolVersion, sessionProtocolStateless)
	}
}

// --- legacy-arm control: a client that never sends server/discover ---

// legacyClientTransport wraps the client's transport so that the SDK's
// server/discover probe is answered LOCALLY with method-not-found — the reply a
// pre-SEP-2575 server gives — and never reaches the wire. What the shim sees is
// byte-for-byte a legacy (initialize-handshake) client, which is the only
// shape a go-sdk v1.7.0 client can be coaxed into without its unexported
// test-only version override.
type legacyClientTransport struct{ inner mcp.Transport }

func (t *legacyClientTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c := &legacyClientConn{Connection: conn, synth: make(chan jsonrpc.Message, 1), inbound: make(chan inboundMsg)}
	go c.pump(ctx)
	return c, nil
}

type inboundMsg struct {
	msg jsonrpc.Message
	err error
}

type legacyClientConn struct {
	mcp.Connection
	synth   chan jsonrpc.Message
	inbound chan inboundMsg
}

// pump reads the inner connection on its own goroutine so Read can select
// between real inbound frames and the locally synthesized discover reply.
func (c *legacyClientConn) pump(ctx context.Context) {
	for {
		msg, err := c.Connection.Read(ctx)
		c.inbound <- inboundMsg{msg, err}
		if err != nil {
			return
		}
	}
}

func (c *legacyClientConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case m := <-c.synth:
		return m, nil
	case in := <-c.inbound:
		return in.msg, in.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *legacyClientConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "server/discover" {
		c.synth <- &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found"}}
		return nil
	}
	return c.Connection.Write(ctx, msg)
}

// TestLegacyClientStillReplaysHandshake is the legacy-arm control (AC2 end to
// end): a client on the initialize handshake still gets the full replay — the
// fresh child is initialized under a synthetic id and the client re-reads the
// new tool set — and the shim classifies the session legacy with no listen
// stream. The stateless arm must never capture such a session.
func TestLegacyClientStillReplaysHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	childA := newE2EChild("A", "alpha")
	childB := newE2EChild("B", "beta")
	s := newE2ESession(t, childA, childB)
	s.transport = &legacyClientTransport{inner: s.transport}
	s.start()
	cs := s.connect(ctx)

	if got := cs.InitializeResult().ProtocolVersion; isStatelessVersion(got) {
		t.Fatalf("control client negotiated %q, want a legacy version", got)
	}
	methods := s.upstreamMethods()
	if len(methods) == 0 || methods[0] != "initialize" {
		t.Fatalf("legacy client's first wire frame was %v, want initialize", methods)
	}
	for _, m := range methods {
		if m == "server/discover" || m == methodSubscriptionsListen {
			t.Fatalf("legacy control leaked %s onto the wire: %v", m, methods)
		}
	}
	if got := toolNames(ctx, t, cs); strings.Join(got, ",") != "alpha" {
		t.Fatalf("tools before swap = %v, want [alpha]", got)
	}
	pre := s.publishNow()
	if pre.SessionProtocol != sessionProtocolLegacy || pre.ListenStreamID != "" || !pre.HandshakeDone {
		t.Fatalf("pre-swap snapshot session_protocol=%q listen_stream_id=%q handshake_done=%v, want legacy/none/true", pre.SessionProtocol, pre.ListenStreamID, pre.HandshakeDone)
	}

	post := s.triggerSwap("hash-B")
	if post.SessionProtocol != sessionProtocolLegacy {
		t.Fatalf("post-swap session_protocol = %q, want legacy", post.SessionProtocol)
	}
	// The legacy replay: a synthetic-id initialize the client never sees, then
	// notifications/initialized, and only then the client's own traffic.
	got := childB.sentMethods()
	if len(got) < 2 || got[0] != "initialize" || got[1] != "notifications/initialized" {
		t.Fatalf("fresh child received %v, want [initialize notifications/initialized ...]", got)
	}
	if !strings.Contains(childB.sentFrames()[0], `"id":"fishhawk-shim/replay/1"`) {
		t.Fatalf("replayed initialize did not carry the synthetic id: %s", childB.sentFrames()[0])
	}
	if req := s.awaitListChanged(); req == nil {
		t.Fatal("legacy client never received the post-swap tools/list_changed")
	} else if id := subscriptionIDOf(req); id != "" {
		t.Fatalf("legacy list_changed carried a subscription id %q, want untagged", id)
	}
	if got := toolNames(ctx, t, cs); strings.Join(got, ",") != "beta" {
		t.Fatalf("tools after swap = %v, want [beta]", got)
	}
}
