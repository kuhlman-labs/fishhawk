package main

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestFrameReaderLargeFrameNoTruncation pins that the newline-delimited framing
// reads a >1MiB line intact — the reason frameReader uses bufio.Reader.ReadBytes
// and not bufio.Scanner (whose default 64KiB token cap would truncate it).
func TestFrameReaderLargeFrameNoTruncation(t *testing.T) {
	big := strings.Repeat("x", 1<<20+123)
	input := big + "\n" + "small\n"
	out := make(chan []byte, 4)
	go frameReader(strings.NewReader(input), out)

	first, ok := <-out
	if !ok {
		t.Fatal("channel closed before first frame")
	}
	if len(first) != len(big)+1 { // +1 for the newline preserved verbatim
		t.Fatalf("first frame length = %d, want %d (no truncation)", len(first), len(big)+1)
	}
	second := <-out
	if string(second) != "small\n" {
		t.Fatalf("second frame = %q", second)
	}
	if _, ok := <-out; ok {
		t.Fatal("expected channel to close after EOF")
	}
}

// TestDefaultChildPathResolvesSibling pins the flag-default convention: with no
// --child, the child resolves to fishhawk-mcp next to the shim executable.
func TestDefaultChildPathResolvesSibling(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	want := filepath.Join(filepath.Dir(exe), "fishhawk-mcp")
	if got := defaultChildPath(); got != want {
		t.Fatalf("defaultChildPath = %q, want %q", got, want)
	}
}

// TestParseFlagsDefaults pins the default flag values and the sibling child
// resolution when --child is omitted.
func TestParseFlagsDefaults(t *testing.T) {
	f, err := parseFlags([]string{"fishhawk-mcp-shim"}, os.Stderr)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.pollInterval != 2*time.Second {
		t.Errorf("poll-interval default = %s, want 2s", f.pollInterval)
	}
	if f.quiesceTimeout != 30*time.Second {
		t.Errorf("quiesce-timeout default = %s, want 30s", f.quiesceTimeout)
	}
	if f.child != defaultChildPath() {
		t.Errorf("child default = %q, want sibling %q", f.child, defaultChildPath())
	}
	if f.status || f.staleOnly {
		t.Errorf("diagnostic mode must be off by default (status=%v stale-only=%v)", f.status, f.staleOnly)
	}
	if f.stateDir != resolveStateDir() {
		t.Errorf("state-dir default = %q, want %q", f.stateDir, resolveStateDir())
	}
	if f.staleGrace != 60*time.Second {
		t.Errorf("stale-grace default = %s, want 60s", f.staleGrace)
	}
}

// TestDefaultStateDirMatchesShellConvention pins that Go and zsh agree on the
// default state dir with no configuration: os.TempDir honours $TMPDIR, which is
// the same resolution the shell side's ${TMPDIR:-/tmp} performs.
func TestDefaultStateDirMatchesShellConvention(t *testing.T) {
	want := filepath.Join(os.TempDir(), "fishhawk-mcp-shim")
	if got := defaultStateDir(); got != want {
		t.Fatalf("defaultStateDir = %q, want %q", got, want)
	}
}

// TestResolveStateDirHonoursEnvOverride pins the FISHHAWK_MCP_SHIM_STATE_DIR
// override the shell advisory forwards (see the state-dir coupling note in
// README.md).
func TestResolveStateDirHonoursEnvOverride(t *testing.T) {
	t.Setenv(stateDirEnv, "/tmp/custom-shim-state")
	if got := resolveStateDir(); got != "/tmp/custom-shim-state" {
		t.Fatalf("resolveStateDir = %q, want the env override", got)
	}
	t.Setenv(stateDirEnv, "")
	if got := resolveStateDir(); got != defaultStateDir() {
		t.Fatalf("empty override should fall back to the default, got %q", got)
	}
}

// TestParseFlagsStatusMode pins the diagnostic flag set.
func TestParseFlagsStatusMode(t *testing.T) {
	f, err := parseFlags([]string{
		"fishhawk-mcp-shim", "--status", "--stale-only",
		"--state-dir", "/tmp/x", "--stale-grace", "5s",
	}, os.Stderr)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.status || !f.staleOnly || f.stateDir != "/tmp/x" || f.staleGrace != 5*time.Second {
		t.Fatalf("status flags not parsed: %+v", f)
	}
}

// failReader fails the test if it is ever read: --status must not touch stdin.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("--status read stdin; it must not")
	return 0, io.EOF
}

// TestStatusRendersSupervisorPublishedState is the cross-boundary proof that
// the PRODUCER (a real supervisor publishing through the real writeState) and
// the CONSUMER (the real --status CLI path) agree on the file format — and that
// --status returns exitOK WITHOUT spawning a child or reading stdin.
func TestStatusRendersSupervisorPublishedState(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	childPath := filepath.Join(dir, "child.bin")
	if err := os.WriteFile(childPath, []byte("rebuilt bytes"), 0o755); err != nil { //nolint:gosec // fixture binary
		t.Fatalf("write child fixture: %v", err)
	}

	// A real supervisor publishing through the real state-file writer.
	child := newFake("A", false)
	child.pid = 4711
	sup := newSupervisor(child, func() childTransport { return newFake("B", true) },
		nil, make(chan []byte), io.Discard, io.Discard, 30*time.Second, time.Second)
	sup.childPath = childPath
	sup.publish = func(st swapState) {
		if err := writeState(stateDir, st); err != nil {
			t.Errorf("writeState: %v", err)
		}
	}
	// A swap armed two hours ago that never happened — the #2831 shape.
	sup.armSwap([]byte{0xde, 0xad})
	sup.pendingSince = time.Now().Add(-2 * time.Hour)
	sup.publishState(outcomeDeferredInFlight)

	var out, errOut bytes.Buffer
	rc := run([]string{
		"fishhawk-mcp-shim", "--status", "--stale-only",
		"--state-dir", stateDir, "--stale-grace", "0",
		// A child path that could not possibly start: if --status ever spawned,
		// run() would take the failure path instead of returning exitOK.
		"--child", filepath.Join(dir, "definitely-not-here"),
	}, failReader{t: t}, &out, &errOut)
	if rc != exitOK {
		t.Fatalf("run(--status) = %d, want %d (stderr: %s)", rc, exitOK, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		strconv.Itoa(os.Getpid()),
		strconv.Itoa(child.pid),
		childPath,
		hex.EncodeToString(child.LaunchHash())[:12],
		outcomeDeferredInFlight,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q\n---\n%s", want, got)
		}
	}
	// --status must not write a state file of its own.
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("--status perturbed the state dir: %v", entries)
	}
}

// TestParseFlagsExplicit pins that explicit flags override the defaults.
func TestParseFlagsExplicit(t *testing.T) {
	f, err := parseFlags([]string{
		"fishhawk-mcp-shim",
		"--child", "/opt/fishhawk/bin/fishhawk-mcp",
		"--poll-interval", "500ms",
		"--quiesce-timeout", "10s",
	}, os.Stderr)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.child != "/opt/fishhawk/bin/fishhawk-mcp" {
		t.Errorf("child = %q", f.child)
	}
	if f.pollInterval != 500*time.Millisecond {
		t.Errorf("poll-interval = %s", f.pollInterval)
	}
	if f.quiesceTimeout != 10*time.Second {
		t.Errorf("quiesce-timeout = %s", f.quiesceTimeout)
	}
}

// TestParseFlagsUnknownRejected pins that an unknown flag is a hard error.
func TestParseFlagsUnknownRejected(t *testing.T) {
	if _, err := parseFlags([]string{"fishhawk-mcp-shim", "--nope"}, os.Stderr); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}
