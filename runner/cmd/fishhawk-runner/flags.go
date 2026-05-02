package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/version"
)

// config is the parsed CLI input. Every field has a corresponding
// flag; the action.yml inputs map 1:1 onto the required flags.
//
// E5.2 added the agent-invocation flags (promptFile, workingDir,
// budget). They're optional in the scaffold sense — if promptFile
// is empty, the runner emits a startup line and exits 0, preserving
// the v0.1 dispatch-path probe. Once a prompt is supplied, the
// runner actually invokes Claude Code.
type config struct {
	runID      string
	backendURL string
	workflow   string
	stage      string

	promptFile string
	workingDir string
	maxTokens  int
	timeout    time.Duration
	bundleOut  string
	planOut    string
}

// parseFlags reads args and populates a config. Returns a usage
// error if any required flag is missing or unparseable.
func parseFlags(args []string, w io.Writer) (config, error) {
	fs := flag.NewFlagSet("fishhawk-runner", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(w, "Usage: fishhawk-runner [flags]")
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, "Required flags:")
		fs.PrintDefaults()
	}

	cfg := config{}
	fs.StringVar(&cfg.runID, "run-id", "", "workflow run identifier (UUID)")
	fs.StringVar(&cfg.backendURL, "backend-url", "", "Fishhawk backend URL")
	fs.StringVar(&cfg.workflow, "workflow", "", "workflow ID matching .fishhawk/workflows.yaml")
	fs.StringVar(&cfg.stage, "stage", "", "stage ID within the workflow")

	fs.StringVar(&cfg.promptFile, "prompt-file", "",
		"path to a UTF-8 file containing the prompt; when empty the runner exits 0 without invoking the agent")
	fs.StringVar(&cfg.workingDir, "working-dir", "",
		"agent CWD; defaults to the runner's CWD")
	fs.IntVar(&cfg.maxTokens, "max-tokens", 0,
		"hard cap on agent tokens (input + output); 0 means no cap")
	fs.DurationVar(&cfg.timeout, "timeout", 15*time.Minute,
		"wall-clock cap on agent invocation")
	fs.StringVar(&cfg.bundleOut, "bundle-out", "",
		"path to write the gzipped trace bundle (ADR-007); when empty, events go to stdout as JSONL")
	fs.StringVar(&cfg.planOut, "plan-out", "",
		"path the agent writes its plan artifact to; when set, the runner validates it against standard_v1 after a successful agent invocation")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	if cfg.runID == "" {
		return cfg, fmt.Errorf("missing required --run-id")
	}
	if cfg.backendURL == "" {
		return cfg, fmt.Errorf("missing required --backend-url")
	}
	if cfg.workflow == "" {
		return cfg, fmt.Errorf("missing required --workflow")
	}
	if cfg.stage == "" {
		return cfg, fmt.Errorf("missing required --stage")
	}
	return cfg, nil
}

// runnerVersion returns the build version pulled from
// internal/version. Wrapped here so the main package doesn't import
// internal/version directly — that keeps the cmd surface minimal
// and the version package a true internal detail of the runner
// module.
func runnerVersion() string { return version.Version }
