//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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
					if mode == "leak_grandchild" {
						// Spawn a grandchild that INHERITS stdout and outlives us,
						// then exit. The grandchild keeps the stdout write end open,
						// so an exit signal gated on stdout EOF would never fire.
						gc := exec.Command("sleep", "5")
						gc.Stdout = os.Stdout
						_ = gc.Start()
						os.Exit(0)
					}
					if mode == "echo_env" || mode == "echo_env_crash_once" {
						fakeEchoEnv(line, idRaw, marker, mode)
					} else {
						fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"marker":"%s"}}`+"\n", idRaw, marker)
					}
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

// echoedEnvKeys are the variables an echo_env child reports (#5592): the two
// sourced host keys, the shim's own token, and two keys a fixture .env sets
// that must never reach the child.
var echoedEnvKeys = []string{
	hostLabelKey, hostCapacityKey, "FISHHAWK_API_TOKEN",
	"FISHHAWK_HOST_DISPATCH_STATE_DIR", "FISHHAWK_DOTENV_ONLY_PROBE", "FAKE_MARKER",
}

// envProbe is one echoed variable.
type envProbe struct {
	Set   bool   `json:"set"`
	Value string `json:"value"`
}

// fakeEchoEnv answers a tools/call with what the child's environment holds
// for echoedEnvKeys. FISHHAWK_API_TOKEN's value is echoed only for a test
// fixture token, so a real operator token never reaches a frame (C6). In
// echo_env_crash_once mode a call named "crash" exits 1 the first time, after
// creating FAKE_CRASH_SENTINEL.
func fakeEchoEnv(line []byte, idRaw, marker, mode string) {
	var call struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(line, &call)
	if mode == "echo_env_crash_once" && call.Params.Name == "crash" {
		sentinel := os.Getenv("FAKE_CRASH_SENTINEL")
		if _, err := os.Stat(sentinel); err != nil {
			_ = os.WriteFile(sentinel, nil, 0o600)
			os.Exit(1)
		}
	}
	env := map[string]envProbe{}
	for _, k := range echoedEnvKeys {
		v, ok := os.LookupEnv(k)
		if k == "FISHHAWK_API_TOKEN" && ok && !strings.HasPrefix(v, "fhk_test_") && !strings.HasPrefix(v, "fhk_dotenv_") {
			v = "<withheld>"
		}
		env[k] = envProbe{Set: ok, Value: v}
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(idRaw),
		"result":  map[string]any{"marker": marker, "env": env},
	})
	fmt.Printf("%s\n", b)
}

// writeChildScript writes (atomically, via rename) a shell script at path that
// execs the test binary as a fake child carrying the given marker/mode. Atomic
// rename means the swap flips content in one step, so the watcher's settle
// debounce sees a stable hash rather than a half-written file.
func writeChildScript(t *testing.T, path, marker, mode string) {
	t.Helper()
	writeChildScriptWith(t, path, marker, mode)
}

// writeChildScriptWith is writeChildScript with extra KEY=value assignments
// for the fake child, single-quoted.
func writeChildScriptWith(t *testing.T, path, marker, mode string, extraEnv ...string) {
	t.Helper()
	extra := ""
	for _, kv := range extraEnv {
		k, v, _ := strings.Cut(kv, "=")
		extra += " " + k + "='" + strings.ReplaceAll(v, "'", `'\''`) + "'"
	}
	script := "#!/bin/sh\nGO_FAKE_CHILD=1 FAKE_MARKER=" + marker + " FAKE_MODE=" + mode + extra +
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

	child := newStdioChild(childPath, io.Discard, nil)
	newChild := func() childTransport { return newStdioChild(childPath, io.Discard, nil) }
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

	c := newStdioChild(childPath, io.Discard, nil)
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

// TestExitFiresDespiteGrandchildHoldingStdout pins that the direct child's exit
// is reported off cmd.Wait, NOT stdout EOF: the child spawns a grandchild that
// inherits the stdout write end and outlives it, then exits. Exited must still
// fire — a readLoop gated on pipe EOF would hang until the grandchild died,
// masking the crash and starving orphan recovery.
func TestExitFiresDespiteGrandchildHoldingStdout(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "leak_grandchild")

	c := newStdioChild(childPath, io.Discard, nil)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Drive the handshake so the child is up.
	if err := c.Send([]byte(initReq1 + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-c.Frames():
	case <-time.After(5 * time.Second):
		t.Fatal("no init response from child")
	}

	// This call makes the child spawn a stdout-holding grandchild and exit.
	if err := c.Send([]byte(`{"jsonrpc":"2.0","method":"tools/call","id":2}` + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-c.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("Exited did not fire; a grandchild holding stdout masked the direct child's exit")
	}
	// Reap the lingering grandchild via the process-group SIGKILL escalation.
	c.Terminate(100 * time.Millisecond)
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

// TestPidZeroBeforeStartLivePidAfter pins the childTransport.Pid() seam the
// swap-state snapshot names the child by (#2831): 0 before Start (no process
// exists yet, and a diagnostic must not invent one), the real OS pid after.
func TestPidZeroBeforeStartLivePidAfter(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "P", "")

	c := newStdioChild(childPath, io.Discard, nil)
	if got := c.Pid(); got != 0 {
		t.Fatalf("Pid before Start = %d, want 0", got)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Terminate(time.Second)

	pid := c.Pid()
	if pid <= 0 {
		t.Fatalf("Pid after Start = %d, want a live pid", pid)
	}
	if !processAlive(pid) {
		t.Fatalf("Pid %d reported by the transport is not a live process", pid)
	}
}

// TestRunPublishesStateFileAndCleansUpOnExit drives the REAL run() wiring — a
// real exec'd child, real pipes, the real publish hook writing through
// writeState — and pins the producer half of the #2831 diagnostic end to end:
// a live shim's snapshot names its own child, and the file is removed when the
// shim shuts down cleanly (which is why a LEFTOVER file always means a shim
// that died without returning).
func TestRunPublishesStateFileAndCleansUpOnExit(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "S", "")
	stateDir := filepath.Join(dir, "state")

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	outC := make(chan []byte, 32)
	go frameReader(outR, outC)

	done := make(chan int, 1)
	go func() {
		rc := run([]string{
			"fishhawk-mcp-shim",
			"--child", childPath,
			"--state-dir", stateDir,
			"--poll-interval", "20ms",
			"--env-file=",
		}, inR, outW, io.Discard)
		_ = outW.Close()
		done <- rc
	}()

	if _, err := io.WriteString(inW, initReq1+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	select {
	case f := <-outC:
		if !strings.Contains(string(f), `"id":1`) {
			t.Fatalf("init response: %q", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the initialize response")
	}

	statePath := stateFilePath(stateDir, os.Getpid())
	var st swapState
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(statePath)
		if err == nil && json.Unmarshal(b, &st) == nil && st.HandshakeDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state file %s never reported a completed handshake (last err %v, state %+v)", statePath, err, st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Schema != stateSchema {
		t.Errorf("schema = %q, want %q", st.Schema, stateSchema)
	}
	if st.ChildPath != childPath {
		t.Errorf("child_path = %q, want %q", st.ChildPath, childPath)
	}
	if st.ChildPid <= 0 || !processAlive(st.ChildPid) {
		t.Errorf("child_pid = %d, want a live pid", st.ChildPid)
	}
	if st.ChildLaunchHash != hashOf(t, childPath) {
		t.Errorf("child_launch_hash = %q, want the on-disk hash", st.ChildLaunchHash)
	}
	if st.ShimPid != os.Getpid() {
		t.Errorf("shim_pid = %d, want %d", st.ShimPid, os.Getpid())
	}

	_ = inW.Close()
	select {
	case rc := <-done:
		if rc != exitOK {
			t.Fatalf("run = %d, want %d", rc, exitOK)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after upstream EOF")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state file was not removed on clean shutdown (stat err = %v)", err)
	}
}

// TestRunDegradesWhenStateDirUnusable pins the diagnostic's degrade path
// through the REAL run() wiring: with --state-dir pointing at a regular file,
// every writeState fails, and the contract is that the DIAGNOSTIC degrades, not
// the proxy. Two things are asserted: frames keep flowing (the initialize
// round-trip AND a later tool call both answer), and the warning appears
// EXACTLY ONCE no matter how many poll ticks fire — the `warned` latch in
// main.go, which would otherwise reprint the same line every poll interval for
// the life of the session.
//
// Nothing here asserts that the settle window is long ENOUGH, so a slow runner
// can only make the once-check weaker, never red.
func TestRunDegradesWhenStateDirUnusable(t *testing.T) {
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "D", "")

	// A regular FILE where the state dir should be: MkdirAll cannot proceed, so
	// every publish fails.
	blocked := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	outC := make(chan []byte, 32)
	go frameReader(outR, outC)
	logs := &syncBuf{}

	done := make(chan int, 1)
	go func() {
		rc := run([]string{
			"fishhawk-mcp-shim",
			"--child", childPath,
			"--state-dir", blocked,
			"--poll-interval", "5ms",
			"--env-file=",
		}, inR, outW, logs)
		_ = outW.Close()
		done <- rc
	}()

	expect := func(want string) {
		t.Helper()
		select {
		case f := <-outC:
			if !strings.Contains(string(f), want) {
				t.Fatalf("frame = %q, want it to contain %q", f, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for a frame containing %q (stderr: %s)", want, logs.String())
		}
	}

	if _, err := io.WriteString(inW, initReq1+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	expect(`"id":1`)

	countWarn := func() int {
		return strings.Count(logs.String(), "swap-state snapshot unavailable")
	}
	waitFor(t, func() bool { return countWarn() >= 1 })

	// Let many more poll ticks fire (5ms interval), then pin the latch.
	time.Sleep(250 * time.Millisecond)
	if got := countWarn(); got != 1 {
		t.Fatalf("warning printed %d times, want exactly 1 (stderr: %s)", got, logs.String())
	}

	// The proxy is unaffected: a later request still round-trips.
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","method":"tools/call","id":2}`+"\n"); err != nil {
		t.Fatalf("write tool call: %v", err)
	}
	expect(`"marker":"D"`)
	if got := countWarn(); got != 1 {
		t.Fatalf("warning printed %d times after continued traffic, want exactly 1", got)
	}

	_ = inW.Close()
	select {
	case rc := <-done:
		if rc != exitOK {
			t.Fatalf("run = %d, want %d (stderr: %s)", rc, exitOK, logs.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after upstream EOF")
	}
}

// --- Host settings from the checkout .env (#5592) ---

const (
	fixtureShimToken   = "fhk_test_fixture_token"
	fixtureDotenvToken = "fhk_dotenv_sentinel"
	fixtureDotenvProbe = "from-dotenv"
)

// isolateHostEnv gives the shim a known token and NO host keys, so whatever
// the child reports for them can only have come from the .env (C6). t.Setenv
// registers the restore; the unset follows it, so these tests are not parallel.
func isolateHostEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FISHHAWK_API_TOKEN", fixtureShimToken)
	for _, k := range []string{hostLabelKey, hostCapacityKey, "FISHHAWK_HOST_DISPATCH_STATE_DIR", "FISHHAWK_DOTENV_ONLY_PROBE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// writeFixtureDotenv writes a checkout .env with the two host keys plus a
// token and a probe key that must never reach the child.
func writeFixtureDotenv(t *testing.T, dir, label string, extra ...string) string {
	t.Helper()
	content := "FISHHAWK_HOST_LABEL=" + label + "\n" +
		"FISHHAWK_HOST_CAPACITY=4\n" +
		"FISHHAWK_API_TOKEN=" + fixtureDotenvToken + "\n" +
		"FISHHAWK_DOTENV_ONLY_PROBE=" + fixtureDotenvProbe + "\n"
	for _, line := range extra {
		content += line + "\n"
	}
	return writeDotenv(t, dir, content)
}

// shimConn is a client talking ndjson to a shim, in process (run()) or exec'd.
type shimConn struct {
	t      *testing.T
	in     io.WriteCloser
	outC   chan []byte
	frames []string
}

func (c *shimConn) send(frame string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, frame+"\n"); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// next reads frames until one contains substr.
func (c *shimConn) next(substr string, timeout time.Duration) string {
	c.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-c.outC:
			if !ok {
				c.t.Fatalf("shim closed stdout while waiting for %q", substr)
			}
			c.frames = append(c.frames, string(f))
			if strings.Contains(string(f), substr) {
				return string(f)
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for a frame containing %q", substr)
			return ""
		}
	}
}

// callEnv sends a tools/call named name under id and returns the env the
// echo_env child reported.
func (c *shimConn) callEnv(id int, name string) (string, map[string]envProbe) {
	c.t.Helper()
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":%d,"params":{"name":%q}}`, id, name))
	f := c.next(fmt.Sprintf(`"id":%d,`, id), 10*time.Second)
	var resp struct {
		Result struct {
			Marker string              `json:"marker"`
			Env    map[string]envProbe `json:"env"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(f), &resp); err != nil || resp.Result.Env == nil {
		c.t.Fatalf("call %d: not an echo_env reply: %q (%v)", id, f, err)
	}
	return resp.Result.Marker, resp.Result.Env
}

// assertNoDotenvLeak pins that no frame and no log line carries the fixture
// .env's token or probe value.
func (c *shimConn) assertNoDotenvLeak(logs string) {
	c.t.Helper()
	for _, f := range append(c.frames, logs) {
		for _, bad := range []string{fixtureDotenvToken, fixtureDotenvProbe} {
			if strings.Contains(f, bad) {
				c.t.Errorf("%q leaked into %q", bad, f)
			}
		}
	}
}

// startShimRun drives the REAL run() wiring (flags, provider closure, stdioChild
// env field, real zsh, real exec'd child) over io.Pipe.
func startShimRun(t *testing.T, args ...string) (*shimConn, *syncBuf) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	outC := make(chan []byte, 64)
	go frameReader(outR, outC)
	logs := &syncBuf{}
	done := make(chan int, 1)
	go func() {
		rc := run(append([]string{"fishhawk-mcp-shim"}, args...), inR, outW, logs)
		_ = outW.Close()
		done <- rc
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case rc := <-done:
			if rc != exitOK {
				t.Errorf("run = %d, want %d (stderr: %s)", rc, exitOK, logs.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("run did not return after upstream EOF")
		}
	})
	c := &shimConn{t: t, in: inW, outC: outC}
	c.send(initReq1)
	c.next(`"id":1`, 10*time.Second)
	return c, logs
}

// assertHostEnv pins what the child saw: the host keys as given, the shim's
// own fixture token, and neither .env-only key.
func assertHostEnv(t *testing.T, env map[string]envProbe, label, capacity string) {
	t.Helper()
	if got := env[hostLabelKey]; !got.Set || got.Value != label {
		t.Errorf("child %s = %+v, want %q", hostLabelKey, got, label)
	}
	if got := env[hostCapacityKey]; !got.Set || got.Value != capacity {
		t.Errorf("child %s = %+v, want %q", hostCapacityKey, got, capacity)
	}
	if got := env["FISHHAWK_API_TOKEN"]; !got.Set || got.Value != fixtureShimToken {
		t.Errorf("child FISHHAWK_API_TOKEN = %+v, want the shim's own %q", got, fixtureShimToken)
	}
	for _, k := range []string{"FISHHAWK_DOTENV_ONLY_PROBE", "FISHHAWK_HOST_DISPATCH_STATE_DIR"} {
		if got := env[k]; got.Set {
			t.Errorf("child %s = %+v, want unset: only the two host keys are ever sourced", k, got)
		}
	}
}

// TestStdioChildPassesProvidedEnv is the zsh-free vehicle for the sourcing
// hop (S0-2): a LITERAL env provider reaches a real exec'd child as its whole
// environment. The test process has no host keys and a fixture token, so an
// inherited environment would show neither label nor capacity and would show
// the token.
func TestStdioChildPassesProvidedEnv(t *testing.T) {
	isolateHostEnv(t)
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "L", "echo_env")
	provided := []string{"PATH=" + os.Getenv("PATH"), hostLabelKey + "=literal-label", hostCapacityKey + "=7"}
	c := newStdioChild(childPath, io.Discard, func() []string { return provided })
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Terminate(time.Second)
	recv := func() string {
		t.Helper()
		select {
		case f := <-c.Frames():
			return string(f)
		case <-time.After(5 * time.Second):
			t.Fatal("no frame from child")
			return ""
		}
	}
	if err := c.Send([]byte(initReq1 + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	recv()
	if err := c.Send([]byte(`{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"name":"probe"}}` + "\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	var resp struct {
		Result struct {
			Env map[string]envProbe `json:"env"`
		} `json:"result"`
	}
	if f := recv(); json.Unmarshal([]byte(f), &resp) != nil || resp.Result.Env == nil {
		t.Fatalf("not an echo_env reply: %q", f)
	}
	env := resp.Result.Env
	if got := env[hostLabelKey]; !got.Set || got.Value != "literal-label" {
		t.Errorf("child %s = %+v, want literal-label from the provider", hostLabelKey, got)
	}
	if got := env[hostCapacityKey]; !got.Set || got.Value != "7" {
		t.Errorf("child %s = %+v, want 7 from the provider", hostCapacityKey, got)
	}
	if got := env["FISHHAWK_API_TOKEN"]; got.Set {
		t.Errorf("child FISHHAWK_API_TOKEN = %+v, want unset: the provider's env is the child's whole env", got)
	}
}

func TestShimSourcesHostSettingsFromDotenv(t *testing.T) {
	requireZsh(t)
	isolateHostEnv(t)
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "echo_env")
	envFile := writeFixtureDotenv(t, dir, "dotenv-label")

	c, logs := startShimRun(t, "--child", childPath, "--env-file", envFile, "--state-dir", filepath.Join(dir, "state"), "--poll-interval", "1h")
	_, env := c.callEnv(2, "probe")
	assertHostEnv(t, env, "dotenv-label", "4")
	c.assertNoDotenvLeak(logs.String())
	if !strings.Contains(logs.String(), "env_file="+envFile) {
		t.Errorf("startup line does not name the env file: %s", logs.String())
	}
}

func TestShimExplicitEnvWinsOverDotenv(t *testing.T) {
	requireZsh(t)
	isolateHostEnv(t)
	t.Setenv(hostLabelKey, "explicit-label")
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "echo_env")
	envFile := writeFixtureDotenv(t, dir, "dotenv-label")

	c, logs := startShimRun(t, "--child", childPath, "--env-file", envFile, "--state-dir", filepath.Join(dir, "state"), "--poll-interval", "1h")
	_, env := c.callEnv(2, "probe")
	assertHostEnv(t, env, "explicit-label", "4")
	c.assertNoDotenvLeak(logs.String())
}

// TestShimNeverPassesNonAllowlistedDotenvKeys is the hermetic D2(b) decider: a
// .env that sets other FISHHAWK_* keys reaches the child with only the two
// host keys, and the child keeps the shim's own token.
func TestShimNeverPassesNonAllowlistedDotenvKeys(t *testing.T) {
	requireZsh(t)
	isolateHostEnv(t)
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "echo_env")
	envFile := writeFixtureDotenv(t, dir, "dotenv-label", "FISHHAWK_HOST_DISPATCH_STATE_DIR="+filepath.Join(dir, "hd"))

	c, logs := startShimRun(t, "--child", childPath, "--env-file", envFile, "--state-dir", filepath.Join(dir, "state"), "--poll-interval", "1h")
	_, env := c.callEnv(2, "probe")
	assertHostEnv(t, env, "dotenv-label", "4")
	c.assertNoDotenvLeak(logs.String())
	if strings.Contains(strings.Join(c.frames, ""), filepath.Join(dir, "hd")) {
		t.Errorf("the .env's FISHHAWK_HOST_DISPATCH_STATE_DIR reached a frame")
	}
}

// TestShimRereadsDotenvOnSwap pins D1f for a hot-swap: the swapped-in child is
// spawned with the .env as it reads NOW.
func TestShimRereadsDotenvOnSwap(t *testing.T) {
	requireZsh(t)
	isolateHostEnv(t)
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScript(t, childPath, "A", "echo_env")
	envFile := writeFixtureDotenv(t, dir, "dotenv-label")

	c, logs := startShimRun(t, "--child", childPath, "--env-file", envFile, "--state-dir", filepath.Join(dir, "state"), "--poll-interval", "20ms")
	_, env := c.callEnv(2, "probe")
	assertHostEnv(t, env, "dotenv-label", "4")

	writeFixtureDotenv(t, dir, "dotenv-label-2")
	writeChildScript(t, childPath, "B", "echo_env")
	c.next("notifications/tools/list_changed", 10*time.Second)
	marker, env := c.callEnv(3, "probe")
	if marker != "B" {
		t.Fatalf("post-swap call answered by marker %q, want B", marker)
	}
	assertHostEnv(t, env, "dotenv-label-2", "4")
	c.assertNoDotenvLeak(logs.String())
}

// TestShimRereadsDotenvOnCrashRespawn pins D1f for a crash-respawn (C9).
func TestShimRereadsDotenvOnCrashRespawn(t *testing.T) {
	requireZsh(t)
	isolateHostEnv(t)
	dir := t.TempDir()
	childPath := filepath.Join(dir, "child.sh")
	writeChildScriptWith(t, childPath, "A", "echo_env_crash_once", "FAKE_CRASH_SENTINEL="+filepath.Join(dir, "crashed"))
	envFile := writeFixtureDotenv(t, dir, "dotenv-label")

	c, logs := startShimRun(t, "--child", childPath, "--env-file", envFile, "--state-dir", filepath.Join(dir, "state"), "--poll-interval", "1h")
	_, env := c.callEnv(2, "probe")
	assertHostEnv(t, env, "dotenv-label", "4")

	writeFixtureDotenv(t, dir, "dotenv-label-3")
	c.send(`{"jsonrpc":"2.0","method":"tools/call","id":3,"params":{"name":"crash"}}`)
	c.next("-32603", 10*time.Second)
	c.next("notifications/tools/list_changed", 10*time.Second)
	_, env = c.callEnv(4, "probe")
	assertHostEnv(t, env, "dotenv-label-3", "4")
	c.assertNoDotenvLeak(logs.String())
}

// TestShimMainProcess is the re-exec helper for the default-env-file tests:
// with GO_SHIM_MAIN=1 the (copied) test binary runs the shim's main path with
// NO flags, so the default --child and --env-file resolve from its own path.
func TestShimMainProcess(t *testing.T) {
	if os.Getenv("GO_SHIM_MAIN") != "1" {
		return
	}
	os.Exit(run([]string{"fishhawk-mcp-shim"}, os.Stdin, os.Stdout, os.Stderr))
}

// execCopiedShim lays out <root>/bin/fishhawk-mcp-shim (a COPY of this test
// binary: os.Executable may resolve a symlink) beside an echo_env
// <root>/bin/fishhawk-mcp and a fixture <root>/.env, then execs the copy with
// no --env-file and no --child under a minimal environment.
func execCopiedShim(t *testing.T, root string) (*shimConn, *syncBuf) {
	t.Helper()
	requireZsh(t)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	shim := filepath.Join(bin, "fishhawk-mcp-shim")
	if err := os.WriteFile(shim, b, 0o755); err != nil { //nolint:gosec // copied test binary
		t.Fatalf("copy test binary: %v", err)
	}
	writeChildScript(t, filepath.Join(bin, "fishhawk-mcp"), "M", "echo_env")
	writeFixtureDotenv(t, root, "default-label")

	cmd := exec.Command(shim, "-test.run=^TestShimMainProcess$")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + os.TempDir(),
		"GO_SHIM_MAIN=1",
		stateDirEnv + "=" + filepath.Join(root, "state"),
		"FISHHAWK_API_TOKEN=" + fixtureShimToken,
	}
	logs := &syncBuf{}
	cmd.Stderr = logs
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("exec copied shim: %v", err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		waited := make(chan error, 1)
		go func() { waited <- cmd.Wait() }()
		select {
		case err := <-waited:
			if err != nil {
				t.Errorf("copied shim exited %v (stderr: %s)", err, logs.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("copied shim did not exit after upstream EOF")
		}
	})
	outC := make(chan []byte, 64)
	go frameReader(out, outC)
	c := &shimConn{t: t, in: in, outC: outC}
	c.send(initReq1)
	c.next(`"id":1`, 10*time.Second)
	return c, logs
}

// TestShimDefaultEnvFileBesideBinDir pins the default (C9, S0-1): inside a
// fishhawk checkout (.git and a regular scripts/dev beside bin/), the shim
// sources <root>/.env with no flags at all.
func TestShimDefaultEnvFileBesideBinDir(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "dev"), []byte("#!/bin/zsh\n"), 0o700); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	c, logs := execCopiedShim(t, root)
	_, env := c.callEnv(2, "probe")
	assertHostEnv(t, env, "default-label", "4")
	c.assertNoDotenvLeak(logs.String())
}

// TestShimDefaultEnvFileReleaseLayoutSourcesNothing pins S0-1 end to end: a
// release-style layout (no .git or scripts/dev beside bin/) sources nothing
// from the default, and says so once with the --env-file remedy.
func TestShimDefaultEnvFileReleaseLayoutSourcesNothing(t *testing.T) {
	root := t.TempDir()
	c, logs := execCopiedShim(t, root)
	_, env := c.callEnv(2, "probe")
	for _, k := range []string{hostLabelKey, hostCapacityKey} {
		if got := env[k]; got.Set {
			t.Errorf("child %s = %+v, want unset outside a checkout", k, got)
		}
	}
	if got := env["FISHHAWK_API_TOKEN"]; got.Value != fixtureShimToken {
		t.Errorf("child FISHHAWK_API_TOKEN = %+v, want the shim's own", got)
	}
	l := logs.String()
	if strings.Count(l, "skipping the default env file") != 1 || !strings.Contains(l, filepath.Join(root, ".env")) || !strings.Contains(l, "--env-file") {
		t.Errorf("stderr must carry one skip line naming %s and --env-file:\n%s", filepath.Join(root, ".env"), l)
	}
	c.assertNoDotenvLeak(l)
}
