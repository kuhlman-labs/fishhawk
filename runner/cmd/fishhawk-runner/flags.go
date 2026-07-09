package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
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

	// agentBinary / agentVersion are set at runtime (not flags), in run()
	// after parseFlags. agentBinary is the resolved agent CLI executable —
	// the operator override (FISHHAWK_AGENT_BIN / FISHHAWK_CODEX_BIN) when
	// set, else the adapter DefaultBinary — and agentVersion is that
	// binary's probed `--version` line ("unknown" on any probe error). Both
	// are recorded on the runner_started log line for provenance (#1741).
	agentBinary  string
	agentVersion string

	// agent selects the coding-agent provider the runner invokes
	// (E22.X / #839). Maps 1:1 onto an agent.Invoker via
	// selectInvoker: "claude-code" (default) wires the existing
	// claudecode adapter; "codex" wires a deferred placeholder; any
	// other value fails fast as a category-A runner/agent error. The
	// default preserves the historical Claude-only behavior for
	// invocations that omit the flag. The selected id is also stamped
	// into the trace bundle manifest's Agent field.
	agent string

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

	// verifyCmd is the shell command run as the in-band test gate
	// after the agent exits cleanly. Empty (default) skips the gate.
	// Corresponds to executor.verify.command in the workflow spec;
	// delivered via --verify-cmd until the backend wires the field
	// through the prompt-fetch response.
	verifyCmd     string
	verifyTimeout time.Duration
	// verifyMaxIterations is the verify-fix loop budget. 0 (default)
	// preserves the single-shot demote-on-failure gate; >0 enables the
	// bounded fix loop. Corresponds to executor.verify.max_iterations in
	// the workflow spec; operator override via --verify-max-iterations.
	// Threaded through but not yet consumed.
	verifyMaxIterations int

	// parallelIsolate, when true, keys the per-run lineage worktree on
	// the run's OWN id instead of the shared lineage root (E24.4 / #1144).
	// The MCP fishhawk_run_children driver sets it for the concurrent
	// decomposed children it dispatches so each sibling provisions an
	// ISOLATED checkout (run-<child>) instead of racing the one shared
	// run-<parent> tree; the children already own distinct per-slice
	// sole-writer branches (E24.1, fishhawk/run-<parent>/slice-<n>), so
	// isolated checkouts are the correct shape for parallel. Default off
	// preserves the shared lineage-root worktree for serial decomposition
	// drive and is a no-op for solo runs (which already key on their own id).
	parallelIsolate bool

	// decomposedFromRunID is set at runtime (not a flag) when the
	// fetched prompt reveals that this run is a decomposed child.
	// Drives shared-branch routing in openPRAndShipArtifact.
	decomposedFromRunID string

	// scopeFiles is set at runtime (not a flag) from the fetched
	// prompt's scope_files on implement stages (#581). When non-empty
	// it bounds the implement commit + policy diff to exactly these
	// declared paths; empty falls back to `git add -A`.
	scopeFiles []upload.ScopeFile

	// agentSelfRetry, maxRetriesSnapshot, and retryAttempt are set at
	// runtime from the fetched prompt response (ADR-023). Not CLI flags.
	// agentSelfRetry gates the retry loop; the other two compute the
	// remaining budget (maxRetriesSnapshot - retryAttempt).
	agentSelfRetry     bool
	maxRetriesSnapshot int
	retryAttempt       int

	// commitAuthorName / commitAuthorEmail are set at runtime (not flags)
	// from the fetched prompt's commit_author_name/commit_author_email
	// (#722). When non-empty they override the gitops bot identity so
	// App-backed commits attribute to the App's bot account; empty falls
	// back to gitops.DefaultAuthorName/DefaultAuthorEmail.
	commitAuthorName  string
	commitAuthorEmail string

	// fixup / fixupBranch are set at runtime (not flags) from the fetched
	// prompt's fixup/fixup_branch (sub-plan C / #762). When fixup is true
	// the implement stage commits onto the EXISTING PR branch fixupBranch
	// via gitops RebaseFromRemote and UPDATES the open PR instead of
	// opening a new one (willOpenPR=false semantics). Both zero on a
	// normal implement stage.
	fixup       bool
	fixupBranch string
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

	fs.StringVar(&cfg.agent, "agent", "claude-code",
		"coding-agent provider to invoke (claude-code|codex); defaults to claude-code")
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
	fs.StringVar(&cfg.verifyCmd, "verify-cmd", "",
		"shell command run as the in-band test gate after the agent exits cleanly (executed via sh -c); empty means skip. Corresponds to executor.verify.command in the workflow spec.")
	fs.DurationVar(&cfg.verifyTimeout, "verify-timeout", 0,
		"wall-clock cap on the verify command; 0 means fall back to 10m inside runVerifyGate")
	fs.IntVar(&cfg.verifyMaxIterations, "verify-max-iterations", 0,
		"verify-fix loop budget; 0 (default) preserves the single-shot demote-on-failure gate. Corresponds to executor.verify.max_iterations in the workflow spec.")
	fs.BoolVar(&cfg.parallelIsolate, "parallel-isolate", false,
		"key the per-run worktree on this run's own id instead of the shared lineage root; set by the MCP fishhawk_run_children driver for concurrent decomposed children so each provisions an isolated checkout. Off (default) preserves the shared lineage-root worktree for serial drive (E24.4 / #1144)")

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
// internal/version. Wrapped as a var so tests can override it to
// simulate version mismatches without rebuilding the binary.
var runnerVersion = func() string { return version.Version }

// runnerGitSHA returns the build commit SHA pulled from
// internal/version. Wrapped as a var so tests can override it
// without rebuilding the binary, mirroring runnerVersion.
var runnerGitSHA = func() string { return version.GitSHA }
