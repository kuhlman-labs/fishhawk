//go:build unix

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFakeChildProcess is the re-exec test-helper: when GO_FAKE_CHILD=1 the test
// binary impersonates a minimal ndjson MCP child. The child.sh scripts written
// by writeChildScript exec the test binary in this mode, so the shim drives a
// REAL exec'd child over REAL pipes. os.Exit at the end suppresses the go-test
// summary line that would otherwise corrupt the stdout protocol channel.
func TestFakeChildProcess(t *testing.T) {
	if os.Getenv("GO_FAKE_CHILD") != "1" {
		return
	}
	marker := os.Getenv("FAKE_MARKER")
	mode := os.Getenv("FAKE_MODE")
	if mode == "ignore_sigterm" {
		signal.Ignore(syscall.SIGTERM)
	}
	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			p := peek(line)
			if p.hasMethod() && p.hasID() {
				idRaw := p.idKey()
				if p.method() == "initialize" {
					fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"fake"},"capabilities":{"tools":{"listChanged":true}}}}`+"\n", idRaw)
				} else {
					if mode == "crash_on_call" {
						os.Exit(1)
					}
					fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"marker":"%s"}}`+"\n", idRaw, marker)
				}
			}
		}
		if err != nil {
			if mode == "ignore_sigterm" {
				// A wedged child: ignore both stdin EOF and SIGTERM so only the
				// SIGKILL group escalation can reap it. A sleep loop (not select{},
				// which trips Go's deadlock detector) keeps the process alive.
				for {
					time.Sleep(time.Hour)
				}
			}
			os.Exit(0)
		}
	}
}

// writeChildScript writes (atomically, via rename) a shell script at path that
// execs the test binary as a fake child carrying the given marker/mode. Atomic
// rename means the swap flips content in one step, so the watcher's settle
// debounce sees a stable hash rather than a half-written file.
func writeChildScript(t *testing.T, path, marker, mode string) {
	t.Helper()
	script := "#!/bin/sh\nGO_FAKE_CHILD=1 FAKE_MARKER=" + marker + " FAKE_MODE=" + mode +
		" exec " + strconv.Quote(os.Args[0]) + " -test.run=TestFakeChildProcess\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(script), 0o755); err != nil {
		t.Fatalf("write child script: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename child script: %v", err)
	}
}

// e2eRig drives the real supervisor over real pipes and a real exec'd child.
type e2eRig struct {
	t    *testing.T
	inW  *io.PipeWriter
	outC chan []byte
}

func newE2ERig(t *testing.T, childPath string, poll time.Duration, sleep func(time.Duration)) *e2eRig {
	t.Helper()
	inR, inW := io.Pipe()
	clientIn := make(chan []byte)
	go frameReader(inR, clientIn)
	out := &frameSink{ch: make(chan []byte, 64)}

	child := newStdioChild(childPath, io.Discard)
	newChild := func() childTransport { return newStdioChild(childPath, io.Discard) }
	w := newWatcher(childPath)
	sup := newSupervisor(child, newChild, w, clientIn, out, io.Discard, 5*time.Second, 2*time.Second)
	if sleep != nil {
		sup.sleep = sleep
	}
	ticker := time.NewTicker(poll)
	sup.tick = ticker.C
	go func() {
		_ = sup.run(context.Background())
		ticker.Stop()
	}()
	return &e2eRig{t: t, inW: inW, outC: out.ch}
}

func (r *e2eRig) send(frame string) {
	if _, err := io.WriteString(r.inW, frame+"\n"); err != nil {
		r.t.Fatalf("send: %v", err)
	}
}

func (r *e2eRig) close() { _ = r.inW.Close() }

// waitFor reads client frames until one contains substr or the deadline passes.
func (r *e2eRig) waitFor(substr string, timeout time.Duration) string {
	r.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f := <-r.outC:
			if strings.Contains(string(f), substr) {
				return string(f)
			}
		case <-deadline:
			r.t.Fatalf("timed out waiting for client frame containing %q", substr)
			return ""
		}
	}
}

// TestEndToEndLifecycleRealChild is this PR's done-means analogue of ADR-060's
// rebuild→tool-call round trip: a real exec'd child over real pipes, driven
// through initialize → tool round-trip → on-disk binary swapped to a variant
// with a different marker → watcher-triggered quiesce/swap/replay → next request
// provably answered by the NEW child, with list_changed observed upstream.
func TestEndToEndLifecycleRealChild(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "")

	rig := newE2ERig(t, childPath, 20*time.Millisecond, nil)
	defer rig.close()

	rig.send(initReq1)
	rig.waitFor(`"id":1`, 5*time.Second)

	rig.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	rig.waitFor(`"marker":"A"`, 5*time.Second)

	// Swap the on-disk binary to the B variant; the watcher settles and swaps.
	writeChildScript(t, childPath, "B", "")
	rig.waitFor("notifications/tools/list_changed", 8*time.Second)

	// The next request must be answered by the NEW (B) child.
	rig.send(`{"jsonrpc":"2.0","method":"tools/call","id":3}`)
	rig.waitFor(`"marker":"B"`, 5*time.Second)
}

// TestTerminateEscalatesToSIGKILL pins that a SIGTERM-ignoring child is
// SIGKILL-escalated to its process group after the grace period.
func TestTerminateEscalatesToSIGKILL(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "ignore_sigterm")

	c := newStdioChild(childPath, io.Discard)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Drive one round-trip so the child is up and past signal.Ignore.
	if err := c.Send([]byte(initReq1 + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-c.Frames():
	case <-time.After(5 * time.Second):
		t.Fatal("no init response from child")
	}

	start := time.Now()
	c.Terminate(300 * time.Millisecond)
	select {
	case <-c.Exited():
		if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
			t.Fatalf("child exited in %s; SIGTERM should have been ignored until the SIGKILL escalation", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM-ignoring child was not reaped by the SIGKILL escalation")
	}
}

// TestEndToEndCrashRespawnRealChild pins child-crash respawn over real pipes:
// the crashed call is orphaned upstream and the child respawns through the
// replay path (list_changed observed).
func TestEndToEndCrashRespawnRealChild(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "crash_on_call")

	// No poll-driven swap here; a long poll interval keeps the watcher quiet.
	rig := newE2ERig(t, childPath, time.Hour, func(time.Duration) {})
	defer rig.close()

	rig.send(initReq1)
	rig.waitFor(`"id":1`, 5*time.Second)

	// This tool call crashes the child.
	rig.send(`{"jsonrpc":"2.0","method":"tools/call","id":2}`)
	rig.waitFor("-32603", 5*time.Second)                           // synthesized orphan error
	rig.waitFor("notifications/tools/list_changed", 8*time.Second) // respawn re-established the session
}
