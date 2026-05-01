// Command fishhawk-runner runs an agent under a Fishhawk workflow
// stage and ships the trace.
//
// E5.1 (https://github.com/kuhlman-labs/fishhawk/issues/52) is just
// the scaffold: flag parsing, version, structured logging, exit
// codes. Real work lands in:
//
//   - E5.2 (#29) — Claude Code invocation harness
//   - E5.3 (#30) — full trace capture + bundling
//   - E5.4 (#31) — plan validation against standard_v1
//   - E5.5 (#53) — post-hoc constraint enforcement
//   - E5.6 (#32) — signed trace shipping to backend
//
// The current binary parses its inputs, prints a single startup
// log line acknowledging them, and exits 0. Customers pinning
// `kuhlman-labs/fishhawk/runner@v0.1` will see this as a no-op
// until E5.2 lands.
package main

import (
	"fmt"
	"io"
	"os"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is split out so tests can drive it without exiting the test
// process. Returns the intended process exit code.
func run(args []string, logSink io.Writer) int {
	cfg, err := parseFlags(args, logSink)
	if err != nil {
		// parseFlags already wrote a usage / error message.
		return exitUsage
	}

	logStartup(logSink, cfg)

	// E5.2 onward replaces this no-op with the agent invocation,
	// trace capture, signing, and shipping. Returning 0 here lets
	// customers pin the runner at v0.1 and exercise the dispatch
	// path end-to-end before the runner does real work.
	return exitOK
}

func logStartup(w io.Writer, cfg config) {
	_, _ = fmt.Fprintf(w,
		`{"event":"runner_started","run_id":%q,"workflow":%q,"stage":%q,"backend_url":%q,"version":%q}`+"\n",
		cfg.runID, cfg.workflow, cfg.stage, cfg.backendURL, runnerVersion(),
	)
}
