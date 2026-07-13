// Command fishhawk-mcp-shim is the stdio session-survival supervisor for the
// fishhawk-mcp MCP server (ADR-060 / #1921).
//
// Claude Code owns the MCP server subprocess and does not reconnect a
// restarted stdio server: a `scripts/dev reload` that rebuilds fishhawk-mcp
// leaves the live session pointed at the old binary until the operator runs
// `/mcp` by hand. The shim closes that gap by sitting between the client and
// fishhawk-mcp. It spawns bin/fishhawk-mcp as a child over pipes and passes
// newline-delimited JSON-RPC frames byte-verbatim in both directions, parsing
// ONLY the client's initialize handshake and message ids (for in-flight
// tracking). A content poller (sha-256, never mtime) watches the child binary;
// on a confirmed rebuild it quiesces to zero in-flight requests, swaps in the
// new binary, replays the recorded handshake, and synthesizes
// notifications/tools/list_changed so the client re-reads the tool set — no
// manual /mcp reconnect.
//
// The child connection sits behind a childTransport seam so a later phase can
// substitute a streamable-HTTP upstream (the #655 gateway phase-0 constraint).
//
// scripts/dev integration, banner retirement, and operator re-registration are
// the sibling issue #1922; this binary is not yet wired into the dev loop.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
)

const (
	exitOK      = 0
	exitFailure = 1

	// terminateGrace is the SIGTERM→SIGKILL window applied when a child is
	// swapped out or the shim shuts down.
	terminateGrace = 3 * time.Second
)

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

// shimFlags captures the parsed CLI flags.
type shimFlags struct {
	child          string
	pollInterval   time.Duration
	quiesceTimeout time.Duration
}

// parseFlags parses the shim flags from args[1:]. An empty --child resolves to
// the sibling fishhawk-mcp next to the shim executable, so the standard bin/
// layout works with zero flags.
func parseFlags(args []string, stderr io.Writer) (shimFlags, error) {
	fs := flag.NewFlagSet("fishhawk-mcp-shim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f shimFlags
	fs.StringVar(&f.child, "child", "", "path to the fishhawk-mcp child binary (default: sibling fishhawk-mcp next to this shim)")
	fs.DurationVar(&f.pollInterval, "poll-interval", 2*time.Second, "how often to poll the child binary for a content change")
	fs.DurationVar(&f.quiesceTimeout, "quiesce-timeout", 30*time.Second, "how long to wait for zero in-flight requests before deferring a swap")
	if err := fs.Parse(args[1:]); err != nil {
		return shimFlags{}, err
	}
	if f.child == "" {
		f.child = defaultChildPath()
	}
	return f, nil
}

// defaultChildPath resolves <dir-of-shim-executable>/fishhawk-mcp so the sibling
// bin/ layout needs no --child. It degrades to the bare name (PATH lookup) if
// the executable path is unavailable.
func defaultChildPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "fishhawk-mcp"
	}
	return filepath.Join(filepath.Dir(exe), "fishhawk-mcp")
}

// run is the testable entry point. It wires a stdioChild + watcher + supervisor
// and blocks until the client closes stdin (upstream EOF), then returns.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f, err := parseFlags(args, stderr)
	if err != nil {
		// flag already wrote the error to stderr.
		return exitFailure
	}
	_, _ = fmt.Fprintf(stderr, "fishhawk-mcp-shim: starting (git_sha=%s child=%s poll=%s quiesce=%s)\n",
		version.GitSHA, f.child, f.pollInterval, f.quiesceTimeout)

	ctx := context.Background()

	clientIn := make(chan []byte)
	go frameReader(stdin, clientIn)

	child := newStdioChild(f.child, stderr)
	newChild := func() childTransport { return newStdioChild(f.child, stderr) }
	w := newWatcher(f.child)

	sup := newSupervisor(child, newChild, w, clientIn, stdout, stderr, f.quiesceTimeout, terminateGrace)
	ticker := time.NewTicker(f.pollInterval)
	defer ticker.Stop()
	sup.tick = ticker.C

	if err := sup.run(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk-mcp-shim: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// frameReader reads newline-delimited frames from r and sends each (byte-
// verbatim, including the trailing newline) to out, closing out on EOF. It uses
// bufio.Reader.ReadBytes — never bufio.Scanner — so a >64KiB frame is not
// truncated.
func frameReader(r io.Reader, out chan<- []byte) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			out <- line
		}
		if err != nil {
			close(out)
			return
		}
	}
}
