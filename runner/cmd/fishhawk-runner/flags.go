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

	promptFile      string
	workingDir      string
	maxTokens       int
	timeout         time.Duration
	bundleOut       string
	planOut         string
	constraintsFile string
	checkBaseRef    string
	uploadTrace     bool
	stageID         string
	fetchPrompt     bool

	// Local-runner mode (E22.8 / #406). The runner's GHA-specific
	// assumptions are narrow — `GITHUB_REPOSITORY` and
	// `GITHUB_REF_NAME` env vars on the implement-stage push path.
	// These flags substitute for the env vars when the runner runs
	// outside GHA (operator's workstation, future K8s pods, etc.).
	// Flag-precedence on read: explicit flag > env var > default.
	//
	// `noPR`, when true, skips the implement-stage push + PR open
	// entirely — the trace still uploads, but the working tree
	// stays dirty for the operator to commit themselves. Default
	// posture for local-runner dev loops; GHA runs leave it false.
	githubRepo string
	baseBranch string
	noPR       bool
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
	fs.DurationVar(&cfg.timeout, "timeout", 0,
		"wall-clock cap on agent invocation; 0 means use server-resolved timeout from --fetch-prompt; falls back to 15m if the server also returns 0")
	fs.StringVar(&cfg.bundleOut, "bundle-out", "",
		"path to write the gzipped trace bundle (ADR-007); when empty, events go to stdout as JSONL")
	fs.StringVar(&cfg.planOut, "plan-out", "",
		"path the agent writes its plan artifact to; when set, the runner validates it against standard_v1 after a successful agent invocation")
	fs.StringVar(&cfg.constraintsFile, "constraints-file", "",
		"path to a JSON file describing the stage's constraints (forbidden_paths, allowed_paths, max_files_changed, required_outcomes); requires --check-base-ref to be useful")
	fs.StringVar(&cfg.checkBaseRef, "check-base-ref", "",
		"git ref to diff against for constraint evaluation (e.g. origin/main); when set together with --constraints-file the runner enforces post-hoc")
	fs.BoolVar(&cfg.uploadTrace, "upload-trace", false,
		"after the agent succeeds, issue a signing key from --backend-url and POST the bundle to /v0/runs/{run_id}/trace")
	fs.StringVar(&cfg.stageID, "stage-id", "",
		"stage UUID for trace upload (distinct from --stage which is the workflow-spec stage name); required with --upload-trace")
	fs.BoolVar(&cfg.fetchPrompt, "fetch-prompt", false,
		"fetch the constructed prompt from --backend-url's /v0/stages/{stage-id}/prompt before invoking the agent; --prompt-file wins when both are set, useful for local replay")
	fs.StringVar(&cfg.githubRepo, "github-repo", "",
		"GitHub repo as owner/name for the implement-stage push + PR open path; falls back to GITHUB_REPOSITORY env when empty (E22.8 / #406)")
	fs.StringVar(&cfg.baseBranch, "base-branch", "",
		"base branch for the implement-stage PR; falls back to GITHUB_REF_NAME env then to 'main' when both empty")
	fs.BoolVar(&cfg.noPR, "no-pr", false,
		"skip the implement-stage git push + PR open; the working tree stays dirty for the operator to commit themselves. Default posture for local-runner dev loops")

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
