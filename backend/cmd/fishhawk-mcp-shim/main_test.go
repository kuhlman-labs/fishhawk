package main

import (
	"os"
	"path/filepath"
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
