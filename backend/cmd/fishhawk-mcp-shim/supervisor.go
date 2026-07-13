package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Synthesized frames the shim injects. Each carries its own trailing newline so
// it round-trips through the same newline-delimited framing as passthrough.
var (
	initializedNotification = []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	listChangedNotification = []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` + "\n")
)

// Backoff bounds for crash respawn (capped exponential, reset after a healthy
// interval).
const (
	backoffBase  = 100 * time.Millisecond
	backoffMax   = 5 * time.Second
	healthyReset = 30 * time.Second
)

// supervisor is the proxy core. It reads client frames from clientIn, forwards
// every frame byte-verbatim to the child, and parses just enough JSON to (a)
// record the initialize handshake and (b) track in-flight client->server
// request ids. It owns a single event loop, so all state is single-threaded
// with no locks: the swap admission barrier, in-flight tracking, and handshake
// state are all sequenced by that loop.
type supervisor struct {
	child    childTransport
	newChild func() childTransport
	watcher  *watcher

	clientIn  <-chan []byte // upstream frames from the client (its stdin)
	clientOut io.Writer     // downstream to the client (its stdout) — protocol channel
	logw      io.Writer     // stderr diagnostics only (never clientOut)

	quiesceTimeout time.Duration
	grace          time.Duration

	// Injectable clocks so timing branches test without wall-clock sleeps.
	after func(time.Duration) <-chan time.Time
	sleep func(time.Duration)
	now   func() time.Time

	// tick fires the watcher poll; swapReq is a test-only direct swap injection.
	tick    <-chan time.Time
	swapReq <-chan []byte

	// --- event-loop-owned state ---
	inFlight      map[string]bool
	initReq       []byte // recorded client initialize request (raw bytes)
	initIDKey     string // id key of the recorded initialize request
	initResp      []byte // recorded child initialize response (raw bytes)
	handshakeDone bool   // the initialize response has flowed to the client

	pendingSwapHash []byte // a swap is wanted; nil = none
	quiesceExpired  bool   // active quiesce timed out; wait passively for idle

	replayN     int
	backoffCur  time.Duration
	lastSpawnAt time.Time
}

func newSupervisor(child childTransport, newChild func() childTransport, w *watcher, in <-chan []byte, out, logw io.Writer, quiesce, grace time.Duration) *supervisor {
	return &supervisor{
		child:          child,
		newChild:       newChild,
		watcher:        w,
		clientIn:       in,
		clientOut:      out,
		logw:           logw,
		quiesceTimeout: quiesce,
		grace:          grace,
		after:          time.After,
		sleep:          time.Sleep,
		now:            time.Now,
		inFlight:       map[string]bool{},
		backoffCur:     backoffBase,
	}
}

// run starts the child and drives the proxy until the client closes its stdin
// (clientIn is closed), then terminates the child and returns nil.
func (s *supervisor) run(ctx context.Context) error {
	if err := s.child.Start(ctx); err != nil {
		return fmt.Errorf("start child: %w", err)
	}
	s.lastSpawnAt = s.now()
	if s.watcher != nil {
		s.watcher.setBaseline(s.child.LaunchHash())
	}

	for {
		select {
		case frame, ok := <-s.clientIn:
			if !ok {
				s.child.Terminate(s.grace)
				return nil
			}
			s.handleUpstream(frame)
		case frame := <-s.child.Frames():
			s.handleDownstream(frame)
		case err := <-s.child.Exited():
			s.handleCrash(ctx, err)
		case <-s.tick:
			if s.watcher != nil {
				if changed, h := s.watcher.step(); changed {
					s.pendingSwapHash = h
				}
			}
		case h := <-s.swapReq:
			s.pendingSwapHash = h
		}
		s.maybeSwap(ctx)
	}
}

// handleUpstream forwards a client frame byte-verbatim to the child and records
// the minimum needed: the initialize request (once) and the id of any
// client->server request (a frame with BOTH a method and an id). Notifications
// (method, no id) and client responses to server-originated requests (id, no
// method) are forwarded but never tracked as in-flight.
func (s *supervisor) handleUpstream(frame []byte) {
	p := peek(frame)
	isReq := p.hasMethod() && p.hasID()
	var key string
	if isReq {
		key = p.idKey()
		s.inFlight[key] = true
	}
	if err := s.child.Send(frame); err != nil {
		// The child's stdin is broken — it may have closed stdin yet still be
		// alive, so no Exited fires on its own and the request would otherwise
		// hang forever. Answer any just-registered request with an orphan error so
		// the client is never stranded, then Terminate the child so its Exited
		// drives the crash-recovery respawn. The record below is skipped: an
		// initialize that never reached the child must not be replayed.
		s.logf("send to child failed: %v; terminating for respawn", err)
		if isReq {
			delete(s.inFlight, key)
			s.sendClient(orphanError(key))
		}
		s.child.Terminate(s.grace)
		return
	}
	if isReq && p.method() == "initialize" && s.initReq == nil {
		s.initReq = cloneBytes(frame)
		s.initIDKey = key
	}
}

// handleDownstream forwards a child frame byte-verbatim to the client. A child
// RESPONSE (id, no method, result/error) clears the matching in-flight id and,
// the first time, captures the initialize response and marks the handshake
// done. A child-originated server->client REQUEST (its own method + id, e.g.
// ping/sampling) is passed through without touching in-flight state.
func (s *supervisor) handleDownstream(frame []byte) {
	p := peek(frame)
	if p.isResponse() {
		key := p.idKey()
		delete(s.inFlight, key)
		if !s.handshakeDone && key == s.initIDKey && s.initReq != nil {
			s.initResp = cloneBytes(frame)
			s.handshakeDone = true
		}
	}
	s.sendClient(frame)
}

// maybeSwap advances a pending swap if one is armed and the handshake is
// complete (a pre-handshake swap is deferred by construction — this returns
// early until handshakeDone). It engages the admission barrier at the instant
// in-flight reaches zero: while quiescing and swapping it never reads clientIn,
// so incoming client frames wait in the channel and are flushed to the NEW
// child only after the replayed handshake completes.
func (s *supervisor) maybeSwap(ctx context.Context) {
	if s.pendingSwapHash == nil || !s.handshakeDone {
		return
	}
	if len(s.inFlight) == 0 {
		s.doSwap(ctx)
		return
	}
	if s.quiesceExpired {
		// Passive wait: swap on the next in-flight==0 transition, handled when a
		// future downstream response empties inFlight and re-enters maybeSwap.
		return
	}
	// Active quiesce: hold upstream (do not read clientIn) and drain in-flight
	// until idle or the quiesce timeout, whichever comes first.
	timeout := s.after(s.quiesceTimeout)
	for len(s.inFlight) > 0 {
		select {
		case frame := <-s.child.Frames():
			s.handleDownstream(frame)
		case err := <-s.child.Exited():
			s.handleCrash(ctx, err)
			return
		case <-timeout:
			s.quiesceExpired = true
			s.logf("quiesce timed out after %s with %d in-flight; deferring swap to next idle", s.quiesceTimeout, len(s.inFlight))
			return
		}
	}
	s.doSwap(ctx)
}

// doSwap terminates the running child and brings up the new binary through the
// replay path. Precondition: in-flight == 0 and handshake complete.
func (s *supervisor) doSwap(ctx context.Context) {
	newHash := s.pendingSwapHash
	s.pendingSwapHash = nil
	s.quiesceExpired = false
	s.logf("child content changed; quiesced at 0 in-flight, swapping")
	old := s.child
	old.Terminate(s.grace)
	// Keep draining the terminating child's frames until it exits. A child that
	// stays chatty between quiesce and process death would otherwise wedge its
	// readLoop on the full (256) Frames buffer, leaking the goroutine and an
	// unreaped process. Draining to exit makes that structurally impossible.
	s.drainOldChild(old)
	s.spawnAndReplay(ctx, newHash)
}

// drainOldChild discards frames from a terminated child until it exits, so a
// child that keeps emitting frames after Terminate cannot block its readLoop on
// a full Frames() buffer. It runs in the background and returns once Exited
// fires (Frames() is never closed — Exited is the authoritative death signal).
func (*supervisor) drainOldChild(c childTransport) {
	go func() {
		for {
			select {
			case <-c.Frames():
			case <-c.Exited():
				return
			}
		}
	}()
}

// handleCrash reacts to the child exiting without a shim-initiated terminate:
// it reaps the crash (orphan errors), backs off, and respawns through the
// replay path. reapCrash and applyCrashBackoff are split out so the replay path
// can drive the SAME respawn iteratively (see spawnAndReplay) rather than
// re-entering handleCrash — a persistently crashing replacement must not recurse
// one stack frame per crash.
func (s *supervisor) handleCrash(ctx context.Context, err error) {
	s.reapCrash(err)
	s.applyCrashBackoff()
	s.spawnAndReplay(ctx, nil)
}

// reapCrash answers every in-flight request with a synthesized JSON-RPC error so
// the client is never stranded, resets in-flight tracking, and cancels any
// mid-flight swap (the respawn re-execs the same on-disk path, which already
// carries the new content).
func (s *supervisor) reapCrash(err error) {
	s.logf("child exited unexpectedly (%v); respawning", err)
	for id := range s.inFlight {
		// A crash before the handshake completes leaves the client's initialize
		// in flight. It is REPLAYED with its original id by spawnAndReplay (the
		// pre-handshake safe-restart path), not orphaned — an orphan error plus a
		// later real init response would strand/confuse the client.
		if !s.handshakeDone && s.initReq != nil && id == s.initIDKey {
			continue
		}
		s.sendClient(orphanError(id))
	}
	s.inFlight = map[string]bool{}
	s.pendingSwapHash = nil
	s.quiesceExpired = false
}

// applyCrashBackoff sleeps for the current capped-exponential backoff and
// advances it, resetting to the base after a healthy interval since the last
// spawn.
func (s *supervisor) applyCrashBackoff() {
	if s.now().Sub(s.lastSpawnAt) > healthyReset {
		s.backoffCur = backoffBase
	}
	s.sleep(s.backoffCur)
	s.backoffCur *= 2
	if s.backoffCur > backoffMax {
		s.backoffCur = backoffMax
	}
}

// terminateAndDrain kills a child whose stdin broke mid-replay (a send error
// leaves it possibly-alive, so no Exited fires on its own) and drains its frames
// to exit so a chatty terminating child cannot wedge its readLoop.
func (s *supervisor) terminateAndDrain(c childTransport) {
	c.Terminate(s.grace)
	s.drainOldChild(c)
}

// spawnAndReplay starts a fresh child and re-establishes the session per the
// pre-initialization-safe-restart rules:
//
//   - no initialize recorded            → plain passthrough (nothing to replay);
//   - initialize recorded, no response  → re-send the ORIGINAL initialize with
//     its original client id so the response flows to the waiting client
//     naturally (no synthetic id, no swallow);
//   - handshake complete                → full replay: re-send initialize with a
//     synthetic collision-proof id, swallow the response, send
//     notifications/initialized, then synthesize notifications/tools/list_changed
//     upstream.
func (s *supervisor) spawnAndReplay(ctx context.Context, newHash []byte) {
	// Retry the WHOLE respawn (Start AND the replayed handshake) in a
	// bounded-stack LOOP rather than self-recursion: a persistently missing,
	// unexecutable, or mid-handshake-crashing child would otherwise recurse one
	// stack frame per backoff interval and eventually exhaust the stack. Backoff
	// is capped; the loop exits as soon as the session is re-established (or ctx
	// is cancelled).
	for {
		var nc childTransport
		for {
			nc = s.newChild()
			err := nc.Start(ctx)
			if err == nil {
				break
			}
			s.logf("respawn start failed: %v; retrying after backoff", err)
			s.sleep(s.backoffCur)
			s.backoffCur *= 2
			if s.backoffCur > backoffMax {
				s.backoffCur = backoffMax
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		s.child = nc
		s.lastSpawnAt = s.now()
		if newHash == nil {
			newHash = nc.LaunchHash()
		}
		if s.watcher != nil {
			s.watcher.setBaseline(newHash)
		}

		// Re-establish the session per the pre-init-safe-restart rules. Each arm
		// yields done=true on success; a send failure (broken stdin) or a
		// mid-handshake crash yields done=false so the loop respawns with capped
		// backoff instead of recursing.
		done := false
		switch {
		case s.initReq == nil:
			// Nothing recorded yet — resume plain passthrough.
			done = true
		case !s.handshakeDone:
			// Pre-handshake restart: re-send the original initialize verbatim so its
			// response reaches the still-waiting client. The main loop captures the
			// response and marks the handshake done as usual.
			if err := s.child.Send(s.initReq); err != nil {
				s.logf("pre-handshake initialize re-send failed: %v; respawning", err)
				s.terminateAndDrain(s.child)
			} else {
				done = true
			}
		default:
			done = s.replayHandshake(ctx)
		}
		if done {
			return
		}
		// The fresh child failed the re-send/replay. Respawn with capped backoff
		// unless the context is cancelled.
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.applyCrashBackoff()
		newHash = nil // recompute the launch hash from the next child
	}
}

// replayHandshake performs the full (post-handshake) replay against a fresh
// child: a synthetic-id initialize whose response is swallowed, then
// notifications/initialized to the child and a synthesized
// notifications/tools/list_changed to the client. It returns true once the
// replay completes, or false when the child cannot be handshaked (a send error
// against a broken stdin, or a crash mid-handshake) — the caller's respawn loop
// then retries with capped backoff. A false return NEVER leaves the client
// hanging on a never-arriving response.
func (s *supervisor) replayHandshake(ctx context.Context) bool {
	s.replayN++
	synthID := fmt.Sprintf("fishhawk-shim/replay/%d", s.replayN)
	if err := s.child.Send(rewriteInitID(s.initReq, synthID)); err != nil {
		// The fresh child's stdin is already broken: swallowResponse would block
		// forever waiting for a response that can never come. Terminate for a
		// respawn instead of entering the read.
		s.logf("replay initialize send failed: %v; respawning", err)
		s.terminateAndDrain(s.child)
		return false
	}
	if !s.swallowResponse(ctx, synthID) {
		// The fresh child died mid-handshake; swallowResponse reaped it. The child
		// is already gone, so do not terminate/drain here — the caller respawns.
		return false
	}
	if err := s.child.Send(initializedNotification); err != nil {
		s.logf("replay initialized send failed: %v; respawning", err)
		s.terminateAndDrain(s.child)
		return false
	}
	s.sendClient(listChangedNotification)
	return true
}

// swallowResponse reads child frames until the response to the synthetic-id
// initialize arrives and discards it. Any other frame in that window (a child
// should emit nothing before it is initialized) is forwarded verbatim. Returns
// false if the child exits mid-handshake, having reaped the crash so the caller
// loop respawns (it does NOT re-enter handleCrash — that would recurse).
func (s *supervisor) swallowResponse(ctx context.Context, synthID string) bool {
	want := strconv.Quote(synthID)
	for {
		select {
		case frame := <-s.child.Frames():
			p := peek(frame)
			if p.isResponse() && p.idKey() == want {
				return true
			}
			s.sendClient(frame)
		case err := <-s.child.Exited():
			s.reapCrash(err)
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func (s *supervisor) sendClient(frame []byte) {
	_, _ = s.clientOut.Write(frame)
}

func (s *supervisor) logf(format string, args ...any) {
	if s.logw == nil {
		return
	}
	_, _ = fmt.Fprintf(s.logw, "fishhawk-mcp-shim: "+format+"\n", args...)
}

// orphanError builds a JSON-RPC error response for a request left in-flight by
// a child crash. idRaw is the request's raw id token (a number or a quoted
// string), which is valid JSON in the id position.
func orphanError(idRaw string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + idRaw + `,"error":{"code":-32603,"message":"fishhawk-shim: child restarted before this request completed"}}` + "\n")
}

// rewriteInitID re-marshals the recorded initialize with a new id. This is the
// only frame the shim ever reformats, and only for the child-facing synthetic
// replay — the client never sees it.
func rewriteInitID(initReq []byte, id string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(initReq, &m); err != nil {
		// Should not happen (initReq was a parsed request), but degrade to the
		// original bytes rather than dropping the handshake.
		return initReq
	}
	m["id"] = json.RawMessage(strconv.Quote(id))
	out, err := json.Marshal(m)
	if err != nil {
		return initReq
	}
	return append(out, '\n')
}

// peek is the minimal JSON-RPC view the supervisor parses per frame. A frame
// that fails to parse (a >1MiB non-JSON payload, malformed JSON) yields a zero
// peek — not a request and not a response — so it is forwarded byte-verbatim
// and never tracked.
type rpcPeek struct {
	Method *string         `json:"method"`
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func peek(frame []byte) rpcPeek {
	var p rpcPeek
	// Unmarshal tolerates the trailing \n (and any \r) the framing preserves.
	_ = json.Unmarshal(frame, &p)
	return p
}

func (p rpcPeek) hasMethod() bool { return p.Method != nil }
func (p rpcPeek) method() string {
	if p.Method == nil {
		return ""
	}
	return *p.Method
}

func (p rpcPeek) hasID() bool {
	t := bytes.TrimSpace(p.ID)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// isResponse reports whether the frame is a response to a request: an id, no
// method, and a result or error member.
func (p rpcPeek) isResponse() bool {
	return !p.hasMethod() && p.hasID() && (len(p.Result) > 0 || len(p.Error) > 0)
}

// idKey is the raw id token, trimmed, used as the in-flight map key. The same
// peer round-trips an id byte-consistently (a numeric 1 stays 1, a string "a"
// stays "a"), so raw-token comparison matches request to response.
func (p rpcPeek) idKey() string {
	return string(bytes.TrimSpace(p.ID))
}
