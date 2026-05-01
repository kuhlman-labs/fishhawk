package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/kuhlman-labs/fishhawk/runner/internal/version"
)

// config is the parsed CLI input that future stages will consume.
// Every field has a corresponding flag; the action.yml inputs map
// 1:1 onto these.
type config struct {
	runID      string
	backendURL string
	workflow   string
	stage      string
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
