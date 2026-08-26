package main

import (
	"context"
	"errors"
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
	pid         int // injectable marker pid returned by Pid()
	autoRespond bool
	failStart   bool // when set, Start returns an error instead of launching

	frames chan []byte
	exited chan error

	mu         sync.Mutex
	sent       [][]byte
	started    bool
	terminated bool
	failSend   bool // when set, Send returns an error and delivers nothing
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
	if f.failSend {
		f.mu.Unlock()
		return errors.New("send failed: " + f.marker)
	}
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

// Pid satisfies the childTransport seam with an injectable marker pid, so the
// supervisor's snapshot can be asserted without an os/exec child.
func (f *fakeChild) Pid() int { return f.pid }

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

// setFailSend flips the child's stdin to broken so subsequent Sends error.
func (f *fakeChild) setFailSend(v bool) {
	f.mu.Lock()
	f.failSend = v
	f.mu.Unlock()
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

// syncBuf is a mutex-guarded log sink: the supervisor writes from its event
// loop while the test reads from its own goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// pubLog captures every published swap-state snapshot. Publishes happen on the
// event-loop goroutine, so access is mutex-guarded.
type pubLog struct {
	mu     sync.Mutex
	states []swapState
}

func (p *pubLog) add(st swapState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states = append(p.states, st)
}

// count returns how many published snapshots satisfy pred.
func (p *pubLog) count(pred func(swapState) bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, st := range p.states {
		if pred(st) {
			n++
		}
	}
	return n
}

// waitFor blocks until a published snapshot satisfies pred, returning it, or
// fails the test on timeout.
func (p *pubLog) waitFor(t *testing.T, what string, pred func(swapState) bool) swapState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for i := len(p.states) - 1; i >= 0; i-- {
			if pred(p.states[i]) {
				st := p.states[i]
				p.mu.Unlock()
				return st
			}
		}
		p.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("timed out waiting for a published snapshot with %s; got %+v", what, p.states)
	return swapState{}
}

// fakeClock is an injectable, mutex-guarded clock for the deferral rate limiter.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t    *testing.T
	sup  *supervisor
	in   chan []byte
	out  *frameSink
	swap chan []byte
	tick chan time.Time
	pub  *pubLog
	log  *syncBuf
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
	logs := &syncBuf{}
	pub := &pubLog{}
	sup := newSupervisor(child0, factory, nil, in, out, logs, 30*time.Second, time.Second)
	sup.swapReq = swap
	sup.publish = pub.add
	tick := make(chan time.Time)
	sup.tick = tick
	return &harness{t: t, sup: sup, in: in, out: out, swap: swap, tick: tick, pub: pub, log: logs}
}

func (h *harness) start() {
	go func() { _ = h.sup.run(context.Background()) }()
}

func (h *harness) send(frame string)       { h.in <- []byte(frame + "\n") }
func (h *harness) sendRaw(frame []byte)    { h.in <- frame }
func (h *harness) triggerSwap(hash string) { h.swap <- []byte(hash) }

// pollTick fires one watcher poll through the real tick path.
func (h *harness) pollTick() { h.tick <- time.Time{} }

// logs returns everything the supervisor has written to its stderr writer.
func (h *harness) logs() string { return h.log.String() }

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

// --- a persistently crashing replacement respawns in a bounded loop (no unbounded recursion) ---

func TestSwapReplacementCrashesRepeatedlyRecoversBounded(t *testing.T) {
	child0 := newFake("A", true)
	// Three replacement children die the instant they are handed the replayed
	// handshake; the fourth completes it. The OLD recursion (swallowResponse →
	// handleCrash → spawnAndReplay → replayHandshake → swallowResponse) added a
	// stack frame per crash; this asserts the loop recovers with capped backoff.
	c1 := newFake("B", false)
	c2 := newFake("C", false)
	c3 := newFake("D", false)
	c4 := newFake("E", true)
	h := newHarness(t, child0, c1, c2, c3, c4)

	var mu sync.Mutex
	var slept []time.Duration
	h.sup.sleep = func(d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	}
	h.start()
	h.handshake()

	// Arm each replacement to die when it is asked to handshake.
	c1.crash(errors.New("die1"))
	c2.crash(errors.New("die2"))
	c3.crash(errors.New("die3"))
	h.triggerSwap("hash-B")

	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed once a replacement finally handshakes, got %q", lc)
	}
	if len(c4.sentFrames()) == 0 {
		t.Fatal("the child that finally handshakes should have received the replayed handshake")
	}
	mu.Lock()
	defer mu.Unlock()
	// One capped backoff sleep per crashed replay before the recovery.
	if len(slept) < 3 {
		t.Fatalf("expected a capped backoff sleep per crashed replay, got %v", slept)
	}
	for i, d := range slept {
		if d > backoffMax {
			t.Fatalf("backoff sleep %d exceeded the cap %s: %v", i, backoffMax, d)
		}
	}
}

// --- upstream Send failure orphans the request and terminates for respawn ---

func TestUpstreamSendFailureOrphansAndRespawns(t *testing.T) {
	child0 := newFake("A", true)
	child1 := newFake("B", true) // the respawn after the broken child is reaped
	h := newHarness(t, child0, child1)
	h.sup.sleep = func(time.Duration) {}
	h.start()
	h.handshake()

	// The child's stdin breaks. A client request must NOT hang: it gets an orphan
	// error and the child is terminated so its exit drives the respawn.
	child0.setFailSend(true)
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":7}`)

	orphan := h.expect()
	if !strings.Contains(orphan, `"id":7`) || !strings.Contains(orphan, "-32603") {
		t.Fatalf("expected an orphan error for id 7 on send failure, got %q", orphan)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !child0.isTerminated() {
		time.Sleep(5 * time.Millisecond)
	}
	if !child0.isTerminated() {
		t.Fatal("a child with broken stdin must be terminated to trigger respawn")
	}

	// The Terminate would fire Exited on a real child; simulate that exit so the
	// crash-recovery respawn re-establishes the session on a healthy child.
	child0.crash(nil)
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed after the send-failure respawn, got %q", lc)
	}
	if len(child1.sentFrames()) == 0 {
		t.Fatal("the respawned child should have received the replayed handshake")
	}
}

// --- a Send failure during the replayed handshake respawns instead of hanging ---

func TestReplaySendFailureRespawns(t *testing.T) {
	child0 := newFake("A", true)
	child1 := newFake("B", true) // fresh child, but its stdin is broken for the replay
	child2 := newFake("C", true) // the retry child completes the replay
	child1.failSend = true
	h := newHarness(t, child0, child1, child2)
	h.sup.sleep = func(time.Duration) {}
	h.start()
	h.handshake()

	h.triggerSwap("hash-B")

	// replayHandshake's initialize Send fails against child1; the shim must NOT
	// block waiting on a response that can never arrive — it terminates child1 and
	// respawns child2, which completes the replay.
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected list_changed after a replay send-failure respawn, got %q", lc)
	}
	if !child1.isTerminated() {
		t.Fatal("the replay child with broken stdin should be terminated")
	}
	if len(child2.sentFrames()) == 0 {
		t.Fatal("the retry child should have received the replayed handshake")
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

// --- #2831: the swap gate no longer defers forever on an unmatched handshake ---

// TestSwapProceedsOnServedRequestEvidence is the AC1/AC4 done-means: a session
// whose initialize response was never MATCHED (the child answered under a
// different id) but whose child has demonstrably served results must still
// swap. The state is reached the way it is reached in the field — a
// pre-handshake crash clears in-flight and re-sends the original initialize,
// and the replacement answers under an id the shim cannot match — so no stored
// initialize RESPONSE exists at swap time.
//
// It also pins the arm the swap takes: the FULL replay (synthetic id, response
// swallowed), NOT the pre-handshake arm's verbatim re-send, which would deliver
// a second response under the client's own JSON-RPC id and corrupt the session.
func TestSwapProceedsOnServedRequestEvidence(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", false) // answers with a MISMATCHED id
	child2 := newFake("C", true)  // the swap target: auto-answers the synthetic replay
	h := newHarness(t, child0, child1, child2)
	h.sup.sleep = func(time.Duration) {}
	h.start()

	// initialize is recorded, then the child dies before answering it.
	h.send(initReq1)
	child0.crash(errors.New("pre-handshake boom"))

	// child1 gets the original initialize re-sent, but answers under an id the
	// shim cannot match: handshakeDone stays false and no initResp is stored.
	waitFor(t, func() bool { return len(child1.sentFrames()) > 0 })
	child1.pushFrame(`{"jsonrpc":"2.0","id":"1-coerced","result":{"serverInfo":{"name":"fake"}}}` + "\n")
	if r := h.expect(); !strings.Contains(r, "1-coerced") {
		t.Fatalf("expected the mismatched init response to flow through, got %q", r)
	}

	// The client's normal traffic is served fine — that is the evidence.
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	waitFor(t, func() bool { return len(child1.sentFrames()) > 1 })
	child1.pushFrame(`{"jsonrpc":"2.0","id":2,"result":{"marker":"B"}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"marker":"B"`) {
		t.Fatalf("expected the served result, got %q", r)
	}

	// A confirmed content change now arrives. Before #2831 this deferred forever.
	h.triggerSwap("hash-C")

	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected the swap to complete with list_changed, got %q", lc)
	}
	if !child1.isTerminated() {
		t.Fatal("the old child should have been terminated by the swap")
	}

	sent := child2.sentFrames()
	var sawSynthetic, sawInitialized, sawVerbatim bool
	for _, f := range sent {
		if strings.Contains(f, "fishhawk-shim/replay/") {
			sawSynthetic = true
		}
		if strings.Contains(f, "notifications/initialized") {
			sawInitialized = true
		}
		if strings.Contains(f, `"method":"initialize"`) && strings.Contains(f, `"id":1,`) {
			sawVerbatim = true
		}
	}
	if !sawSynthetic || !sawInitialized {
		t.Fatalf("expected the FULL replay against the new child, got %v", sent)
	}
	if sawVerbatim {
		t.Fatalf("the presumed-handshake swap must NOT re-send the ORIGINAL initialize (it would duplicate a response under the client's own id): %v", sent)
	}
	// The swallowed synthetic response must not reach the client.
	h.expectNone()

	st := h.pub.waitFor(t, "handshake_presumed", func(st swapState) bool { return st.HandshakePresumed })
	if !st.HandshakeDone {
		t.Error("a presumed handshake must also read handshake_done (it is what selects the full replay arm)")
	}
	if st.ServedResults < handshakeEvidenceThreshold {
		t.Errorf("served_results = %d, want at least %d", st.ServedResults, handshakeEvidenceThreshold)
	}
}

// waitFor spins until cond holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a condition")
}

// TestSwapStillDeferredWithoutServedEvidence pins that the fallback is EVIDENCE
// based, not a blanket permission: an initialize recorded but never answered,
// with zero served results, still defers — and now SAYS SO.
func TestSwapStillDeferredWithoutServedEvidence(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.start()

	h.send(initReq1) // recorded; the child never answers
	waitFor(t, func() bool { return len(child0.sentFrames()) > 0 })

	h.triggerSwap("hash-B")
	st := h.pub.waitFor(t, "deferred_handshake_not_observed", func(st swapState) bool {
		return st.LastSwapOutcome == outcomeDeferredHandshakeNotSeen
	})
	if st.HandshakePresumed {
		t.Error("handshake must not be presumed without a served result")
	}
	if child0.isTerminated() {
		t.Fatal("a swap with no handshake evidence must be deferred, not executed")
	}
}

// TestSwapDeferredWhenNoInitializeRecorded pins the refusal arm: without a
// recorded initialize there is nothing to replay, so a fresh child would never
// be initialized. The swap is refused AND the refusal is reported — the one
// path where the operator is told "why not" instead of getting a swap.
func TestSwapDeferredWhenNoInitializeRecorded(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.start()

	// A live child serving results, with NO initialize ever recorded.
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":5}`)
	waitFor(t, func() bool { return len(child0.sentFrames()) > 0 })
	child0.pushFrame(`{"jsonrpc":"2.0","id":5,"result":{"marker":"A"}}` + "\n")
	if r := h.expect(); !strings.Contains(r, `"marker":"A"`) {
		t.Fatalf("served result: %q", r)
	}

	h.triggerSwap("hash-B")
	st := h.pub.waitFor(t, "deferred_no_initialize_recorded", func(st swapState) bool {
		return st.LastSwapOutcome == outcomeDeferredNoInitialize
	})
	if st.HandshakePresumed {
		t.Error("handshake must not be presumed with no initialize recorded")
	}
	if child0.isTerminated() {
		t.Fatal("a swap with no recorded initialize must be refused, not executed")
	}
	waitFor(t, func() bool { return strings.Contains(h.logs(), "no client initialize was ever recorded") })
}

// TestPresumedHandshakeRetiresLeakedInitialize is the NO-CRASH mismatched
// initialize: the child never dies, it simply answers the client's initialize
// under a different id. handleUpstream registered the original id as in flight
// and handleDownstream can only clear it by MATCHING, so that entry leaks for
// the life of the session.
//
// TestSwapProceedsOnServedRequestEvidence cannot cover this — its preliminary
// pre-handshake crash runs reapCrash, which resets both in-flight maps, so by
// the time its gate presumes the handshake there is nothing left to leak. Here
// the leak is live at swap time, and before the fix the gate opened only for
// maybeSwap to see a permanently non-empty in-flight set, enter the
// deferred_in_flight passive wait, and never swap again.
//
// The assertion is the COMPLETED full-replay swap, not the internal map: the
// swap must finish, take the synthetic-id arm, and publish in_flight 0.
func TestPresumedHandshakeRetiresLeakedInitialize(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true) // the swap target: auto-answers the synthetic replay
	h := newHarness(t, child0, child1)
	h.start()

	// The client initializes. No crash, ever.
	h.send(initReq1)
	waitFor(t, func() bool { return len(child0.sentFrames()) > 0 })

	// The child answers under an id the shim cannot match: handshakeDone stays
	// false, no initResp is stored, and in-flight id "1" is never cleared. The
	// result-bearing response is the served evidence the gate keys on.
	child0.pushFrame(`{"jsonrpc":"2.0","id":"1-coerced","result":{"serverInfo":{"name":"fake"}}}` + "\n")
	if r := h.expect(); !strings.Contains(r, "1-coerced") {
		t.Fatalf("expected the mismatched init response to flow through, got %q", r)
	}
	// The leaked initialize is observable in the published snapshot before the
	// swap is even armed — the state this test exists to survive.
	h.pollTick()
	if st := h.pub.waitFor(t, "the leaked initialize in flight", func(st swapState) bool {
		return st.InFlight == 1 && st.OldestInFlightID == "1"
	}); st.HandshakeDone {
		t.Fatalf("the mismatched response must NOT complete the handshake: %+v", st)
	}

	h.triggerSwap("hash-B")

	// The swap must COMPLETE. Before the fix this timed out: the gate presumed
	// the handshake and maybeSwap then parked in the passive wait forever.
	lc := h.expect()
	if !strings.Contains(lc, "notifications/tools/list_changed") {
		t.Fatalf("expected the swap to complete with list_changed, got %q", lc)
	}
	if !child0.isTerminated() {
		t.Fatal("the old child should have been terminated by the swap")
	}

	// The full-replay arm, not the pre-handshake verbatim re-send (which would
	// deliver a second response under the client's own id).
	sent := child1.sentFrames()
	var sawSynthetic, sawVerbatim bool
	for _, f := range sent {
		if strings.Contains(f, "fishhawk-shim/replay/") {
			sawSynthetic = true
		}
		if strings.Contains(f, `"method":"initialize"`) && strings.Contains(f, `"id":1,`) {
			sawVerbatim = true
		}
	}
	if !sawSynthetic {
		t.Fatalf("expected the FULL replay against the new child, got %v", sent)
	}
	if sawVerbatim {
		t.Fatalf("the presumed-handshake swap must NOT re-send the ORIGINAL initialize: %v", sent)
	}
	h.expectNone()

	st := h.pub.waitFor(t, "the completed swap", func(st swapState) bool {
		return st.LastSwapOutcome == outcomeSwapped
	})
	if !st.HandshakePresumed {
		t.Error("the swap must have gone through the presumption arm")
	}
	if st.InFlight != 0 {
		t.Errorf("in_flight = %d after handshake presumption, want 0: the never-matched initialize must not stay outstanding", st.InFlight)
	}
	if st.OldestInFlightID != "" {
		t.Errorf("oldest_in_flight_id = %q, want empty", st.OldestInFlightID)
	}
}

// TestErrorOnlyResponsesAreNotHandshakeEvidence pins that evidence means a
// SUCCESSFUL result, not any response frame: a child answering only JSON-RPC
// errors has not demonstrated a completed initialize lifecycle.
func TestErrorOnlyResponsesAreNotHandshakeEvidence(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.start()

	h.send(initReq1)
	waitFor(t, func() bool { return len(child0.sentFrames()) > 0 })
	// Mismatched-id error response: not a handshake match, and not evidence.
	child0.pushFrame(`{"jsonrpc":"2.0","id":"1-coerced","error":{"code":-32603,"message":"nope"}}` + "\n")
	h.expect()
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	waitFor(t, func() bool { return len(child0.sentFrames()) > 1 })
	child0.pushFrame(`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"still nope"}}` + "\n")
	h.expect()

	h.triggerSwap("hash-B")
	st := h.pub.waitFor(t, "deferred_handshake_not_observed", func(st swapState) bool {
		return st.LastSwapOutcome == outcomeDeferredHandshakeNotSeen
	})
	if st.ServedResults != 0 {
		t.Errorf("served_results = %d, want 0 (error-only responses are not evidence)", st.ServedResults)
	}
	if child0.isTerminated() {
		t.Fatal("error-only responses must not unlock the swap")
	}
}

// TestQuiesceDeferralPublishesOldestInFlight pins the in-flight leak
// instrumentation: a swap held off by a long-running request names WHICH
// request and for how long, in the snapshot and in the log. In-flight requests
// are deliberately never force-orphaned — legitimate stdio calls in this repo
// block for hours — so visibility is the whole remedy.
func TestQuiesceDeferralPublishesOldestInFlight(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	timeoutCh := make(chan time.Time, 1)
	h.sup.after = func(time.Duration) <-chan time.Time { return timeoutCh }
	h.start()

	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	h.expect()

	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":7}`)
	timeoutCh <- time.Time{}
	h.triggerSwap("hash-B")

	st := h.pub.waitFor(t, "deferred_in_flight", func(st swapState) bool {
		return st.LastSwapOutcome == outcomeDeferredInFlight
	})
	if st.InFlight != 1 {
		t.Errorf("in_flight = %d, want 1", st.InFlight)
	}
	if st.OldestInFlightID != "7" {
		t.Errorf("oldest_in_flight_id = %q, want \"7\"", st.OldestInFlightID)
	}
	if st.OldestInFlightSince == "" {
		t.Error("oldest_in_flight_since must be stamped")
	}
	if child0.isTerminated() {
		t.Fatal("a deferral must never kill the in-flight child")
	}
	waitFor(t, func() bool {
		l := h.logs()
		return strings.Contains(l, "oldest id 7") && strings.Contains(l, "age ")
	})
}

// TestPassiveWaitRepublishesOnTickOnlyNotPerFrame pins the tick gate on the
// passive-wait republish. maybeSwap re-enters after EVERY select branch, so an
// ungated republish is one atomic state-file write per PROXIED FRAME for as
// long as a long-lived request holds the swap off — hours, by design, since
// in-flight requests are never force-orphaned. The watcher tick is what keeps
// the snapshot fresh (fixed interval, traffic-independent), so only the tick
// publishes.
//
// The counterfactual: with the fromTick guard removed, the five notifications
// below each add a publish and the exact-count assertion goes red.
func TestPassiveWaitRepublishesOnTickOnlyNotPerFrame(t *testing.T) {
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	timeoutCh := make(chan time.Time, 1)
	h.sup.after = func(time.Duration) <-chan time.Time { return timeoutCh }
	h.start()

	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	h.expect()

	// A request that never completes, then the quiesce timeout: the swap parks
	// in the passive-wait arm.
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":7}`)
	timeoutCh <- time.Time{}
	h.triggerSwap("hash-B")
	isDeferral := func(st swapState) bool { return st.LastSwapOutcome == outcomeDeferredInFlight }
	h.pub.waitFor(t, "deferred_in_flight", isDeferral)

	all := func(swapState) bool { return true }
	before := h.pub.count(all)

	// Five proxied client frames. Each is an UNBUFFERED handoff to the event
	// loop, so by the time the last send returns every earlier iteration —
	// including its maybeSwap re-entry — has already run. Notifications carry no
	// id, so none of them changes in-flight state.
	for i := 0; i < 5; i++ {
		h.send(`{"jsonrpc":"2.0","method":"notifications/progress"}`)
	}

	// One tick: publishes exactly twice (the tick's own freshness publish, then
	// the deferral publish that overwrites the outcome).
	h.pollTick()
	waitFor(t, func() bool { return h.pub.count(all) >= before+2 })
	if got := h.pub.count(all); got != before+2 {
		t.Fatalf("publishes after 5 proxied frames + 1 tick = %d, want %d (the frames must not republish)", got-before, 2)
	}

	// And the TICK publish is the one carrying the deferral fields (C3): the
	// passive-wait arm changes no state, so nothing else would refresh them.
	st := h.pub.waitFor(t, "deferred_in_flight after the tick", isDeferral)
	if st.OldestInFlightID != "7" || st.OldestInFlightSince == "" || st.InFlight != 1 {
		t.Fatalf("tick publish lost the deferral fields: %+v", st)
	}
	if child0.isTerminated() {
		t.Fatal("a deferral must never kill the in-flight child")
	}
}

// TestDeferralLogRateLimited pins that a persistent deferral logs on the
// transition and then at most once per deferralLogInterval, while EVERY
// re-entry republishes the snapshot. The re-entries are driven by watcher
// TICKS, because the passive-wait arm returns without changing state — the tick
// publish is the only thing keeping the operator-readable channel fresh.
func TestDeferralLogRateLimited(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)}
	child0 := newFake("A", false)
	child1 := newFake("B", true)
	h := newHarness(t, child0, child1)
	h.sup.now = clock.now
	timeoutCh := make(chan time.Time, 1)
	h.sup.after = func(time.Duration) <-chan time.Time { return timeoutCh }
	h.start()

	h.send(initReq1)
	child0.pushFrame(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")
	h.expect()
	h.send(`{"jsonrpc":"2.0","method":"tools/call","id":3}`)
	timeoutCh <- time.Time{}
	h.triggerSwap("hash-B")

	isDeferral := func(st swapState) bool { return st.LastSwapOutcome == outcomeDeferredInFlight }
	h.pub.waitFor(t, "deferred_in_flight", isDeferral)
	countLogs := func() int { return strings.Count(h.logs(), "swap deferred: quiesce timeout") }
	if got := countLogs(); got != 1 {
		t.Fatalf("log lines after the first deferral = %d, want 1", got)
	}
	before := h.pub.count(isDeferral)

	// Two ticks within the interval: republished every time, logged neither.
	h.pollTick()
	h.pollTick()
	waitFor(t, func() bool { return h.pub.count(isDeferral) >= before+2 })
	if got := countLogs(); got != 1 {
		t.Fatalf("log lines while rate-limited = %d, want 1", got)
	}

	// Past the interval: the same persistent denial logs once more.
	clock.advance(deferralLogInterval + time.Second)
	h.pollTick()
	waitFor(t, func() bool { return countLogs() == 2 })
	if got := countLogs(); got != 2 {
		t.Fatalf("log lines after the interval elapsed = %d, want 2", got)
	}
	if child0.isTerminated() {
		t.Fatal("the child must still be alive: the request never completed")
	}
}
