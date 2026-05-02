// Command fishhawk-runner runs an agent under a Fishhawk workflow
// stage and ships the trace.
//
// E5.1 (#52) shipped the scaffold. E5.2 (#29) wires the Claude Code
// invocation harness: when --prompt-file is supplied, the runner
// invokes Claude Code, captures the trace, and emits it as JSON
// Lines on stdout. Trace bundling (E5.3 / #30) and shipping to the
// backend (E5.6 / #32) replace the stdout emission later.
//
// Without --prompt-file the binary parses its inputs, prints a
// single startup log line, and exits 0. Customers pinning
// `kuhlman-labs/fishhawk/runner@v0.1` see this no-op until the
// downstream stages of E5 land.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/claudecode"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// newInvoker is the seam tests use to swap the real Claude Code
// adapter for a fake. Production wiring is the only assignment in
// non-test code; tests reassign and restore it via t.Cleanup.
var newInvoker = func(apiKey string) agent.Invoker {
	return claudecode.New(apiKey)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run is split out so tests can drive it without exiting the test
// process. Returns the intended process exit code.
//
// logSink receives the structured startup log line and any failure
// notes; trace events (when the harness runs) go to os.Stdout so
// they can be piped or captured separately. This split lets a
// caller redirect stderr for diagnostics while keeping the trace
// stream clean.
func run(args []string, logSink io.Writer) int {
	cfg, err := parseFlags(args, logSink)
	if err != nil {
		// parseFlags already wrote a usage / error message.
		return exitUsage
	}

	logStartup(logSink, cfg)

	if cfg.promptFile == "" {
		// Scaffold mode preserved: --prompt-file unset means
		// "exercise the dispatch path; do not invoke the agent."
		return exitOK
	}

	prompt, err := os.ReadFile(cfg.promptFile)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"runner_failed","reason":"read_prompt","detail":%q}`+"\n", err.Error())
		return exitUsage
	}

	inv := agent.Invocation{
		RunID:      cfg.runID,
		Stage:      cfg.stage,
		Prompt:     string(prompt),
		WorkingDir: cfg.workingDir,
		Budget: agent.Budget{
			MaxTokens: cfg.maxTokens,
			Timeout:   cfg.timeout,
		},
	}

	invoker := newInvoker(os.Getenv("ANTHROPIC_API_KEY"))
	res, invokeErr := invoker.Invoke(context.Background(), inv)

	emitEvents(os.Stdout, res.Events)
	logCompletion(logSink, res, invokeErr)

	if !res.OK {
		return exitFailure
	}
	return exitOK
}

func logStartup(w io.Writer, cfg config) {
	_, _ = fmt.Fprintf(w,
		`{"event":"runner_started","run_id":%q,"workflow":%q,"stage":%q,"backend_url":%q,"version":%q,"prompt_file":%q}`+"\n",
		cfg.runID, cfg.workflow, cfg.stage, cfg.backendURL, runnerVersion(), cfg.promptFile,
	)
}

// logCompletion writes a single structured line summarizing the
// invocation outcome. Mirrors the failure categories from
// MVP_SPEC §6 so log scrapers can switch on `category`.
func logCompletion(w io.Writer, res agent.Result, err error) {
	if res.OK {
		_, _ = fmt.Fprintf(w,
			`{"event":"runner_completed","outcome":"ok","tokens_used":%d}`+"\n",
			res.TokensUsed)
		return
	}
	reason := res.FailureReason
	if reason == "" && err != nil {
		reason = err.Error()
	}
	category := res.FailureCategory
	if category == "" {
		category = "A"
	}
	_, _ = fmt.Fprintf(w,
		`{"event":"runner_completed","outcome":"failed","category":%q,"reason":%q,"tokens_used":%d,"err_class":%q}`+"\n",
		category, reason, res.TokensUsed, classifyErr(err))
}

// classifyErr returns a stable short label for the wrapped agent
// error so log consumers don't have to substring-match prose.
func classifyErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, agent.ErrTimeout):
		return "timeout"
	case errors.Is(err, agent.ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, agent.ErrBinaryNotFound):
		return "binary_not_found"
	case errors.Is(err, agent.ErrAgentFailed):
		return "agent_failed"
	default:
		return "other"
	}
}

// emitEvents writes one JSON object per line. This is the
// placeholder transport — E5.3 / #30 replaces it with the
// JSONL.gz bundle format and E5.6 / #32 with the signed upload.
func emitEvents(w io.Writer, events []agent.Event) {
	enc := json.NewEncoder(w)
	for _, ev := range events {
		_ = enc.Encode(ev)
	}
}
