// Command fishhawk-runner runs an agent under a Fishhawk workflow
// stage and ships the trace.
//
// E5.1 (#52) shipped the scaffold. E5.2 (#29) wired the Claude Code
// invocation harness. E5.3 (#30) added trace bundling: when
// --prompt-file and --bundle-out are supplied together, the runner
// invokes the agent, packs the captured events into the *.jsonl.gz
// wire format from ADR-007, and writes the bundle to disk. Signed
// upload to the backend lands in E5.6 (#32).
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
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
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

	if cfg.bundleOut != "" {
		if err := writeBundle(cfg, res); err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"runner_failed","reason":"bundle_write","detail":%q}`+"\n", err.Error())
			logCompletion(logSink, res, invokeErr)
			return exitFailure
		}
	} else {
		// No bundle path configured — fall back to JSONL on stdout
		// so callers exercising --prompt-file alone can still
		// inspect the captured trace. E5.6 will replace this with
		// signed upload to the backend.
		emitEvents(os.Stdout, res.Events)
	}

	logCompletion(logSink, res, invokeErr)

	if !res.OK {
		return exitFailure
	}
	return exitOK
}

// writeBundle packs the captured events into the ADR-007 wire
// format and writes the gzipped bytes to cfg.bundleOut. Returns the
// storage hash via a side channel — for now we just log it in
// logCompletion when a bundle was written; E5.6 will hand it to the
// signing layer + backend upload.
func writeBundle(cfg config, res agent.Result) error {
	data, _, err := bundle.PackBytes(bundle.PackInputs{
		RunID:   cfg.runID,
		StageID: cfg.stage, // stage UUID not yet plumbed; v0.x replaces with cfg.stageID
		Agent:   "claude-code",
	}, res.Events)
	if err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	// 0o600 — bundle may carry redacted credentials in raw events
	// until E2.4 redaction is wired upstream of the bundler. The
	// runner's filesystem is ephemeral, but defense in depth is
	// cheap.
	if err := os.WriteFile(cfg.bundleOut, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", cfg.bundleOut, err)
	}
	return nil
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
