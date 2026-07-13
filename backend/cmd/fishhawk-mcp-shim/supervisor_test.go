package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChild is an in-process childTransport. The supervisor drives it through
// the same seam as the real stdioChild and never learns it is not os/exec —
// which is exactly what makes these tests the transport-seam contract proof (a
// streamable-HTTP upstream could slot in the same way, per #655 phase 0).
type fakeChild struct {
	marker      string
	hash        []byte
	autoRespond bool
	failStart   bool // when set, Start returns an error instead of launching

	frames chan []byte
	exited chan error

	mu         sync.Mutex
	sent       [][]byte
	started    bool
	terminated bool
}

func newFake(marker string, autoRespond bool) *fakeChild {
	return &fakeChild{
		marker:      marker,
		hash:        []byte("hash-" + marker),
		autoRespond: autoRespond,
		frames:      make(chan []byte, 256),
		exited:      make(chan error, 1),
	}
}

func (f *fakeChild) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart {
		return errors.New("start failed: " + f.marker)
	}
	f.started = true
	return nil
}

func (f *fakeChild) Send(frame []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, cloneBytes(frame))
	auto := f.autoRespond
	f.mu.Unlock()
	if !auto {
		return nil
	}
	p := peek(frame)
	if !p.hasMethod() || !p.hasID() {
		return nil
	}
	idRaw := p.idKey()
	var resp string
	if p.method() == "initialize" {
		resp = `{"jsonrpc":"2.0","id":` + idRaw + `,"result":{"serverInfo":{"name":"fake"},"capabilities":{"tools":{"listChanged":true}}}}`
	} else {
		resp = `{"jsonrpc":"2.0","id":` + idRaw + `,"result":{"marker":"` + f.marker + `"}}`
	}
	f.frames <- []byte(resp + "\n")
	return nil
}

func (f *fakeChild) Frames() <-chan []byte { return f.frames }
func (f *fakeChild) Exited() <-chan error  { return f.exited }
func (f *fakeChild) LaunchHash() []byte    { return f.hash }

func (f *fakeChild) Terminate(grace time.Duration) {
	f.mu.Lock()
	f.terminated = true
	f.mu.Unlock()
}

func (f *fakeChild) isTerminated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminated
}

func (f *fakeChild) sentFrames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, b := range f.sent {
		out[i] = string(b)
	}
	return out
}

// pushFrame injects a downstream frame verbatim (caller supplies any newline).
func (f *fakeChild) pushFrame(s string) { f.frames <- []byte(s) }

// crash simulates an unexpected child exit.
func (f *fakeChild) crash(err error) { f.exited <- err }

// frameSink collects downstream (client-facing) frames. sendClient always
// writes exactly one whole frame per call, so one Write == one frame.
type frameSink struct{ ch chan []byte }

func (s *frameSink) Write(p []byte) (int, error) {
	s.ch <- cloneBytes(p)
	return len(p), nil
}

type harness struct {
	t    *testing.T
	sup  *supervisor
	in   chan []byte
	out  *frameSink
	swap chan []byte
}

// newHarness wires a supervisor over the given first child plus a factory queue
// of subsequent children. It does NOT start the loop — configure sup.after /
// sup.sleep first, then call start().
func newHarness(t *testing.T, child0 *fakeChild, rest ...*fakeChild) *harness {
	t.Helper()
	in := make(chan []byte)
	out := &frameSink{ch: make(chan []byte, 256)}
	swap := make(chan []byte)
	idx := 0
	factory := func() childTransport {
		if idx >= len(rest) {
			panic("fake factory exhausted")
		}
		c := rest[idx]
		idx++
		return c
	}
	sup := newSupervisor(child0, factory, nil, in, out, io.Discard, 30*time.Second, time.Second)
	sup.swapReq = swap
	return &harness{t: t, sup: sup, in: in, out: out, swap: swap}
}

func (h *harness) start() {
	go func() { _ = h.sup.run(context.Background()) }()
}

func (h *harness) send(frame string)       { h.in <- []byte(frame + "\n") }
func (h *harness) sendRaw(frame []byte)    { h.in <- frame }
func (h *harness) triggerSwap(hash string) { h.swap <- []byte(hash) }

// expect returns the next client-facing frame or fails on timeout.
func (h *harness) expect() string {
	h.t.Helper()
	select {
	case f := <-h.out.ch:
		return string(f)
	case <-time.After(3 * time.Second):
		h.t.Fatal("timed out waiting for a client frame")
		return ""
	}
}

// expectNone asserts no client frame arrives within a short window.
func (h *harness) expectNone() {
	h.t.Helper()
	select {
	case f := <-h.out.ch:
		h.t.Fatalf("expected no client frame, got %q", f)
	case <-time.After(150 * time.Millisecond):
	}
}

const initReq1 = `{"jsonrpc":"2.0","method":"initialize","id":1,"params":{}}`

// handshake drives the initialize round-trip against an auto-responding child
// and returns once the client has received the init response.
func (h *harness) handshake() {
	h.t.Helper()
	h.send(initReq1)
	resp := h.expect()
	if !strings.Contains(resp, `"id":1`) || !strings.Contains(resp, `"result"`) {
		h.t.Fatalf("expected init response for id 1, got %q", resp)
	}
}

// --- peek classification (in-flight tracking correctness) ---

func TestPeekClassification(t *testing.T) {
	cases := []struct {
		name                           string
		frame                          string
		req, resp, notif, childReqLike bool
	}{
		{"numeric-id request", `{"method":"tools/call","id":2}`, true, false, false, false},
		{"string-id request", `{"method":"tools/call","id":"abc"}`, true, false, false, false},
		{"notification", `{"method":"notifications/initialized"}`, false, false, true, false},
		{"response", `{"id":2,"result":{}}`, false, true, false, false},
		{"error response", `{"id":2,"error":{"code":-1}}`, false, true, false, false},
		{"null id notification-like", `{"method":"x","id":null}`, false, false, true, false},
		{"child-originated request", `{"method":"ping","id":"srv-1"}`, true, false, false, true},
		{"garbage", "not json at all", false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := peek([]byte(c.frame))
			isReq := p.hasMethod() && p.hasID()
			if isReq != c.req {
				t.Errorf("isRequest = %v, want %v", isReq, c.req)
			}
			if p.isResponse() != c.resp {
				t.Errorf("isResponse = %v, want %v", p.isResponse(), c.resp)
			}
			isNotif := p.hasMethod() && !p.hasID()
			if isNotif != c.notif {
				t.Errorf("isNotification = %v, want %v", isNotif, c.notif)
			}
			// A child-originated request is method+id but NOT a response, so
			// handleDownstream passes it through without touching in-flight.
			if c.childReqLike && p.isResponse() {
				t.Error("child-originated request must not be classified as a response")
			}
		})
	}
}

// --- byte-verbatim passthrough (both ways, \r\n, >1MiB) ---

func TestPassthroughByteVerbatimBothWays(t *testing.T) {
	child := newFake("A", false)
	h := newHarness(t, child)
	h.start()

	// Init handshake, manual response (child is non-auto so its sent log stays clean).
	h.send(initReq1)
	child.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"id":1`) {
		t.Fatalf("init response: %q", r)
	}

	// Upstream: a >1MiB frame with a \r\n terminator must reach the child byte-
	// for-byte (a bufio.Scanner 64KiB cap would truncate it).
	big := strings.Repeat("x", 1<<20+37)
	upstream := `{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"blob":"` + big + `"}}` + "\r\n"
	h.sendRaw([]byte(upstream))
	// Give the loop a moment to forward.
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		frames := child.sentFrames()
		if len(frames) > 0 {
			got = frames[len(frames)-1]
			if len(got) == len(upstream) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != upstream {
		t.Fatalf("upstream not byte-verbatim: got %d bytes, want %d", len(got), len(upstream))
	}

	// Downstream: a large response with \r\n must reach the client verbatim.
	downstream := `{"jsonrpc":"2.0","id":2,"result":{"blob":"` + big + `"}}` + "\r\n"
	child.pushFrame(downstream)
	if r := h.expect(); r != downstream {
		t.Fatalf("downstream not byte-verbatim: got %d bytes, want %d", len(r), len(downstream))
	}
}

// --- swap admission barrier (condition 1) ---

func TestSwapBuffersFramesUntilNewChildReady(t *testing.T) {
	child0 := newFake("A", false) // manual, so we control in-flight timing
	child1 := newFake("B", true)  // NEW child auto-answers
	h := newHarness(t, child0, child1)
	h.start()

	// Manual handshake.
	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"id":1`) {
		t.Fatalf("init: %q", r)
	}

	// Put a request in flight so the swap enters active quiesce and the event
	// loop stops reading clientIn.
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":9}`)
	// Trigger the swap: it blocks in quiesce (id 9 outstanding) and will not read
	// clientIn until the whole swap/replay completes.
	h.triggerSwap("hash-B")

	// With the loop parked in quiesce, THIS client frame is now provably pending
	// in clientIn BEFORE the swap begins — the send cannot be read until the swap
	// finishes. A missing admission barrier would forward it to the terminating
	// child0 (or answer it before list_changed); the assertions below reject both.
	frameArrived := make(chan struct{})
	go func() {
		h.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)
		close(frameArrived)
	}()

	// The mid-swap frame must stay blocked while the swap is pending.
	select {
	case <-frameArrived:
		t.Fatal("mid-swap client frame was read before the swap completed (no admission barrier)")
	case <-time.After(100 * time.Millisecond):
	}
	// It must NOT have leaked to the terminating child mid-swap either.
	for _, f := range child0.sentFrames() {
		if strings.Contains(f, `"id":2`) {
			t.Fatalf("client frame leaked to the terminating child during swap: %q", f)
		}
	}

	// Complete the in-flight request → quiesce ends → doSwap → replay.
	child0.pushFrame(`{"jsonrpc":"2.0","id":9,"result":{"marker":"A"}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"id":9`) {
		t.Fatalf("expected the in-flight response to flow, got %q", r)
	}
	// End of the replayed handshake.
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed after swap, got %q", lc)
	}
	// Only now is the buffered mid-swap frame flushed to the NEW child (marker B),
	// after the replayed handshake — ordering preserved.
	<-frameArrived
	resp := h.expect()
	if !strings.Contains(resp, `"marker":"B"`) || !strings.Contains(resp, `"id":2`) {
		t.Fatalf("mid-swap request must be answered by the NEW child, got %q", resp)
	}

	// The terminating (old) child must NEVER have received the mid-swap frame.
	for _, f := range child0.sentFrames() {
		if strings.Contains(f, `"id":2`) {
			t.Fatalf("client frame leaked to the terminating child: %q", f)
		}
	}
	if !child0.isTerminated() {
		t.Fatal("old child should be terminated after swap")
	}
}

// --- full replay: synthetic id, response swallow, notifications/initialized ordering ---

func TestSwapReplaysHandshakeWithSyntheticID(t *testing.T) {
	child0 := newFake("A", true)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.start()
	h.handshake()

	h.triggerSwap("hash-B")
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed, got %q", lc)
	}

	sent := child1.sentFrames()
	if len(sent) < 2 {
		t.Fatalf("new child should have received replayed initialize + initialized, got %v", sent)
	}
	// First frame: the replayed initialize carries the synthetic id (outside the
	// client's id space) and NOT the client's original id 1.
	if !strings.Contains(sent[0], "fishhawk-shim/replay/") {
		t.Fatalf("replayed initialize must use a synthetic id, got %q", sent[0])
	}
	if strings.Contains(sent[0], `"id":1`) {
		t.Fatalf("replayed initialize must not reuse the client id, got %q", sent[0])
	}
	// Second frame: notifications/initialized, sent AFTER the replayed init.
	if !strings.Contains(sent[1], "notifications/initialized") {
		t.Fatalf("expected notifications/initialized after replay, got %q", sent[1])
	}
	// The synthetic init RESPONSE was swallowed — the client never saw it.
	// (Only the list_changed reached the client for this swap.)
	h.expectNone()
}

// --- quiesce holds the swap while a request is in flight, completes at idle ---

func TestQuiesceHoldsSwapUntilIdle(t *testing.T) {
	child0 := newFake("A", false) // manual control of the in-flight response
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.start()

	// Manual init handshake.
	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"id":1`) {
		t.Fatalf("init: %q", r)
	}

	// A request goes in flight and is NOT answered yet.
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	// Trigger the swap; it must block on quiesce, not kill the child.
	h.triggerSwap("hash-B")
	time.Sleep(100 * time.Millisecond)
	if child0.isTerminated() {
		t.Fatal("child must not be terminated while a request is in flight")
	}

	// Complete the in-flight request → in-flight hits 0 → the swap proceeds.
	child0.pushFrame(`{"jsonrpc":"2.0","id":2,"result":{"marker":"A"}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"marker":"A"`) {
		t.Fatalf("expected the in-flight response to flow through, got %q", r)
	}
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("swap should proceed at idle, got %q", lc)
	}
	if !child0.isTerminated() {
		t.Fatal("child should be terminated once idle")
	}
}

// --- quiesce timeout defers the swap to the next idle transition (no mid-request kill) ---

func TestQuiesceTimeoutDefersRatherThanKills(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)

	// Controllable quiesce timeout: a pre-loaded channel fires immediately.
	timeoutCh := make(chan time.Time, 1)
	h.sup.after = func(time.Duration) <-chan time.Time { return timeoutCh }
	h.start()

	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	h.expect()

	// Request in flight; pre-fire the quiesce timeout so the active quiesce
	// times out immediately and defers instead of killing mid-request.
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	timeoutCh <- time.Time{}
	h.triggerSwap("hash-B")

	time.Sleep(100 * time.Millisecond)
	if child0.isTerminated() {
		t.Fatal("quiesce timeout must DEFER the swap, never kill the in-flight child")
	}

	// The request completes → next idle transition → the deferred swap fires.
	child0.pushFrame(`{"jsonrpc":"2.0","id":2,"result":{"marker":"A"}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"marker":"A"`) {
		t.Fatalf("in-flight response: %q", r)
	}
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("deferred swap should fire at idle, got %q", lc)
	}
	if !child0.isTerminated() {
		t.Fatal("child should be terminated after the deferred swap fires")
	}
}

// --- crash respawn: orphan errors, list_changed, capped backoff ---

func TestCrashRespawnOrphansAndBacksOff(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	child2 := newFake("C", true)
	h := newHarness(t, child0, child1, child2)

	var mu sync.Mutex
	var slept []time.Duration
	h.sup.sleep = func(d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	}
	h.start()

	// Handshake (manual), then a request in flight.
	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	h.expect()
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)

	// Crash the child: the in-flight request must get a synthesized error, then
	// the child respawns and re-establishes the session (list_changed).
	child0.crash(errors.New("boom"))
	orphan := h.expect()
	if !strings.Contains(orphan, `"id":2`) || !strings.Contains(orphan, "-32603") {
		t.Fatalf("expected synthesized orphan error for id 2, got %q", orphan)
	}
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed after crash respawn, got %q", lc)
	}

	// A second crash on the respawned child → backoff must grow (capped exp).
	child1.crash(errors.New("boom2"))
	if r := h.expect(); !strings.Contains(r, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed after second respawn, got %q", r)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(slept) < 2 {
		t.Fatalf("expected at least 2 backoff sleeps, got %v", slept)
	}
	if slept[1] <= slept[0] {
		t.Fatalf("backoff must grow: %v then %v", slept[0], slept[1])
	}
	if slept[0] != backoffBase {
		t.Fatalf("first backoff should be the base %s, got %s", backoffBase, slept[0])
	}
}

// --- respawn retries a bounded loop on Start failure (no unbounded recursion) ---

func TestRespawnRetriesOnStartFailure(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	child2 := newFake("C", true)
	child3 := newFake("D", true)
	// The first two respawn attempts fail to Start; the third launches.
	child1.failStart = true
	child2.failStart = true
	h := newHarness(t, child0, child1, child2, child3)

	var mu sync.Mutex
	var sleeps int
	h.sup.sleep = func(time.Duration) {
		mu.Lock()
		sleeps++
		mu.Unlock()
	}
	h.start()

	// Handshake, then crash the child. The respawn must retry Start in a loop
	// (not self-recursion) until child3 launches and re-establishes the session.
	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	h.expect()
	child0.crash(errors.New("boom"))

	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed once a respawn finally starts, got %q", lc)
	}
	if len(child3.sentFrames()) == 0 {
		t.Fatal("the child that finally started should have received the replayed handshake")
	}
	mu.Lock()
	defer mu.Unlock()
	// One backoff from handleCrash + one per failed Start (2) = at least 3.
	if sleeps < 3 {
		t.Fatalf("expected retry-on-start-failure backoff sleeps, got %d", sleeps)
	}
}

// --- the terminating child's frames are drained so a chatty child cannot wedge ---

func TestDrainOldChildConsumesChattyFrames(t *testing.T) {
	c := newFake("old", false)
	sup := &supervisor{}

	// A child that stays chatty after the swap emits far more than the 256-frame
	// buffer. Without draining, the 257th send blocks forever and leaks readLoop.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 512; i++ {
			c.frames <- []byte("noise\n")
		}
		close(done)
	}()

	sup.drainOldChild(c)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("chatty old child blocked; drainOldChild did not consume its frames")
	}
	c.exited <- nil // let the drain goroutine return
}

// --- pre-init safe restart: no initialize recorded → plain passthrough (condition 2a) ---

func TestPreInitCrashNoInitializeRecorded(t *testing.T) {
	child0 := newFake("A", true)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.sup.sleep = func(time.Duration) {}
	h.start()

	// Crash before any initialize: nothing to replay, no orphan, no list_changed.
	child0.crash(errors.New("early death"))
	h.expectNone()

	// Passthrough resumes on the fresh child: a subsequent initialize is
	// answered normally.
	h.send(initReq1)
	if r := h.expect(); !strings.Contains(r, `"id":1`) || !strings.Contains(r, `"result"`) {
		t.Fatalf("post-respawn passthrough init: %q", r)
	}
	if len(child1.sentFrames()) == 0 {
		t.Fatal("fresh child should have received the passthrough initialize")
	}
}

// --- pre-init safe restart: initialize recorded, response not yet arrived → re-send original id (condition 2b) ---

func TestPreInitCrashReplaysOriginalInitialize(t *testing.T) {
	child0 := newFake("A", false) // withhold the init response
	child1 := newFake("B", true)  // fresh child answers the re-sent init
	h := newHarness(t, child0, child1)
	h.sup.sleep = func(time.Duration) {}
	h.start()

	// Client's initialize recorded, but no response yet (handshake incomplete).
	h.send(initReq1)
	time.Sleep(50 * time.Millisecond)

	// Crash mid-handshake.
	child0.crash(errors.New("boom"))

	// The client must NOT get an orphan error for its initialize; instead the
	// fresh child receives the ORIGINAL initialize (original client id 1) and
	// its response flows to the waiting client naturally.
	resp := h.expect()
	if !strings.Contains(resp, `"id":1`) || !strings.Contains(resp, `"result"`) {
		t.Fatalf("expected the init response (original id 1) to flow, got %q", resp)
	}
	if strings.Contains(resp, "-32603") {
		t.Fatalf("initialize must be replayed, not orphaned: %q", resp)
	}
	sent := child1.sentFrames()
	if len(sent) == 0 || !strings.Contains(sent[0], `"id":1`) {
		t.Fatalf("fresh child must receive the ORIGINAL initialize (id 1), got %v", sent)
	}
	if strings.Contains(sent[0], "fishhawk-shim/replay/") {
		t.Fatalf("pre-handshake restart must NOT use a synthetic id, got %q", sent[0])
	}
}

// --- pre-init deferral: a watcher swap arriving pre-handshake is deferred (condition 2c) ---

func TestWatcherSwapDeferredUntilHandshake(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.start()

	// Initialize sent, response not yet arrived.
	h.send(initReq1)
	time.Sleep(30 * time.Millisecond)

	// A watcher-triggered swap arrives pre-handshake → must be deferred.
	h.triggerSwap("hash-B")
	time.Sleep(100 * time.Millisecond)
	if child0.isTerminated() {
		t.Fatal("a pre-handshake swap must be deferred, not executed")
	}

	// Complete the handshake → the deferred swap now fires.
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"id":1`) {
		t.Fatalf("init: %q", r)
	}
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("deferred swap should fire after handshake, got %q", lc)
	}
	if !child0.isTerminated() {
		t.Fatal("child should be terminated once the deferred swap fires")
	}
}

// --- new child dies during the replayed handshake → crash respawn ---

func TestSwapNewChildCrashesDuringHandshake(t *testing.T) {
	child0 := newFake("A", true)
	child1 := newFake("B", false) // will NOT answer the synthetic init; crashes instead
	child2 := newFake("C", true)  // the retry child completes the handshake
	h := newHarness(t, child0, child1, child2)
	h.sup.sleep = func(time.Duration) {}
	h.start()
	h.handshake()

	// Arm child1 to die the instant it is asked to handshake.
	child1.crash(errors.New("died mid-handshake"))
	h.triggerSwap("hash-B")

	// swallowResponse observes the exit, routes a crash respawn, and the retry
	// child (C) completes the replayed handshake — list_changed reaches the client.
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed after the retry respawn, got %q", lc)
	}
	if len(child2.sentFrames()) == 0 {
		t.Fatal("retry child should have received the replayed handshake")
	}
}

// --- clean shutdown on upstream EOF ---

func TestCleanShutdownOnUpstreamEOF(t *testing.T) {
	child0 := newFake("A", true)
	h := newHarness(t, child0)

	done := make(chan struct{})
	go func() { _ = h.sup.run(context.Background()); close(done) }()
	h.handshake()

	close(h.in) // client closed stdin
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not return on upstream EOF")
	}
	if !child0.isTerminated() {
		t.Fatal("child should be terminated on shutdown")
	}
}
