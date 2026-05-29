package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/ghcomment"
)

// runRunner dispatches to `fishhawk runner <subcommand>`. v0 ships
// exactly one verb: `runner start`. The package is structured for
// future siblings (e.g. `runner stop`, `runner doctor`) without a
// flag reshuffle.
func runRunner(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk runner: subcommand required (start)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return runRunnerStart(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk runner: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// runnerStartCommand is the subprocess fishhawk runner start spawns.
// Exposed as a var so tests can substitute a recording fake without
// actually running the runner binary. Production wires `exec.Command`.
var runnerStartCommand = exec.Command

// runnerBinaryResolver looks up the runner binary path. Falls back
// in order: --runner-binary flag > FISHHAWK_RUNNER_BIN env > PATH
// lookup of `fishhawk-runner`. Returns an error with a clean message
// when none resolves. Test seam via `var runnerBinaryLookPath`.
var runnerBinaryLookPath = exec.LookPath

// runnerNewClient is a test seam for runRunnerStart. Production
// wires to newClient; tests swap to point at an httptest.Server.
var runnerNewClient = newClient

// gitRemoteOriginURL returns the configured `origin` remote URL for
// the working directory (or the absolute path it resolves to). Test
// seam — production wires `git remote get-url origin`.
var gitRemoteOriginURL = func(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runRunnerStart implements `fishhawk runner start --run-id … --stage-id …`.
//
// The verb is intentionally thin: it gathers the operator's config
// (backend URL, token, working dir, GitHub repo), composes the
// fishhawk-runner subprocess argv, spawns it, pipes its stdout and
// stderr through, and exits with the runner's exit code. Test seams
// (`runnerStartCommand`, `runnerBinaryLookPath`, `gitRemoteOriginURL`)
// let unit tests assert on the constructed argv without spawning a
// real subprocess.
//
// Per ADR-022's addendum (#388): local-runner runs carry
// runner_kind=local at the backend; the operator-side write tools
// minted the run with that tag via `fishhawk run start
// --runner-kind local` upstream. This verb just invokes the
// runner against an already-minted run.
func runRunnerStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk runner start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	runID := fs.String("run-id", "", "Fishhawk run UUID (required)")
	stageID := fs.String("stage-id", "", "stage UUID inside the run (required)")
	workflow := fs.String("workflow", "", "workflow ID matching the run's workflow (required)")
	stage := fs.String("stage", "", "stage type (plan|implement|review) matching the workflow spec (required)")
	workingDir := fs.String("working-dir", ".", "checkout directory the agent runs in")
	githubRepo := fs.String("github-repo", "",
		"GitHub repo as owner/name; auto-detected from `git remote get-url origin` when empty")
	baseBranch := fs.String("base-branch", "main",
		"base branch for the implement-stage PR (no effect when --no-pr is set)")
	noPR := fs.Bool("no-pr", false,
		"skip implement-stage push + PR open; operator commits the changes themselves (legacy local-mode behavior)")
	runnerBinary := fs.String("runner-binary", envOr("FISHHAWK_RUNNER_BIN", ""),
		"path to the fishhawk-runner binary; defaults to PATH lookup of `fishhawk-runner`")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *runID == "" || *stageID == "" || *workflow == "" || *stage == "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk runner start: --run-id, --stage-id, --workflow, and --stage are required")
		fs.Usage()
		return exitUsage
	}

	// Resolve the runner binary path. --runner-binary > FISHHAWK_RUNNER_BIN > PATH.
	binary := *runnerBinary
	if binary == "" {
		resolved, err := runnerBinaryLookPath("fishhawk-runner")
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "fishhawk runner start: fishhawk-runner not found on PATH; pass --runner-binary or set FISHHAWK_RUNNER_BIN")
			return exitFailure
		}
		binary = resolved
	}

	// Resolve the GitHub repo. Flag wins; otherwise auto-detect
	// from `git remote get-url origin`. Auto-detect failure is a
	// soft failure — the runner can still proceed when --no-pr is
	// set (no push, no PR, no repo lookup needed). For PR-shaped
	// runs without --github-repo and no detectable origin, the
	// runner will surface its own error.
	repo := *githubRepo
	if repo == "" {
		detected, err := detectGitHubRepo(*workingDir)
		switch {
		case err == nil:
			repo = detected
		case *noPR || *stage != "implement":
			// No PR will be opened: either the operator set
			// --no-pr, or this isn't an implement stage. Repo
			// isn't required; skip silently.
		default:
			_, _ = fmt.Fprintf(stderr, "fishhawk runner start: --github-repo not set and could not detect from origin: %v\n", err)
			return exitFailure
		}
	}

	// Build the runner argv. Mirrors the GHA action.yml's inputs
	// closely; the only differences are the local-mode flags
	// landed in E22.8 / #406.
	argv := []string{
		"--run-id", *runID,
		"--backend-url", *cf.backendURL,
		"--workflow", *workflow,
		"--stage", *stage,
		"--stage-id", *stageID,
		"--working-dir", *workingDir,
		"--fetch-prompt",
		"--upload-trace",
	}
	// For plan stages, the agent's prompt instructs it to write
	// the plan to /tmp/fishhawk-plan.json (backend's
	// prompt.PlanArtifactPath). The runner only validates and
	// uploads when --plan-out is set; without it the agent
	// writes the file but the stage never transitions to
	// awaiting_approval. Mirror the GHA action.yml's default so
	// the local loop matches.
	if *stage == "plan" {
		argv = append(argv, "--plan-out", "/tmp/fishhawk-plan.json")
	}
	if repo != "" {
		argv = append(argv, "--github-repo", repo)
	}
	if *baseBranch != "" {
		argv = append(argv, "--base-branch", *baseBranch)
	}
	// Only implement stages produce a diff to enforce. Passing
	// --check-base-ref makes the runner emit the git_diff event the
	// backend needs to re-evaluate policy (policy_evaluated) and run
	// implement-review (#561/#585). Plan/review stages omit it. Mirrors
	// backend/cmd/fishhawk-mcp/run_stage.go — keep in sync.
	if *stage == "implement" {
		argv = append(argv, "--check-base-ref", *baseBranch)
	}
	if *noPR {
		argv = append(argv, "--no-pr")
	}

	cmd := runnerStartCommand(binary, argv...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// FISHHAWK_API_TOKEN is the runner's name for the bearer
	// token; the operator's --token flag carries the same value
	// here. Pass via env (matches how Claude Code's MCP
	// registration passes the token) so the argv stays clean of
	// secrets.
	env := append(os.Environ(),
		"FISHHAWK_API_TOKEN="+*cf.token,
	)
	cmd.Env = env

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	_ = ctx // The runner manages its own timeouts via --timeout; ctx
	// is here so a future enhancement (operator Ctrl-C → propagate
	// to the runner) has a hook.

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Runner ran but failed. Pass through its exit code so
			// shell scripts can branch on it (the runner uses
			// distinct codes per failure category).
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintf(stderr, "fishhawk runner start: spawn failed: %v\n", err)
		return exitFailure
	}

	// #428: after the runner exits cleanly, post or edit the sticky
	// status comment for local-runner issue-triggered runs. Both the
	// auto-PR path and the plain stage-complete path call
	// PostOrEditStatusComment — edit-in-place makes dual calls
	// idempotent. Best-effort; failures don't affect the verb's exit code.
	parsedRunID, perr := uuid.Parse(*runID)
	var parsedStageID uuid.UUID
	var stageParseErr error
	if perr == nil {
		parsedStageID, stageParseErr = uuid.Parse(*stageID)
	}
	if perr == nil && stageParseErr == nil && *stage == "implement" && !*noPR {
		clientCtx, clientCancel := context.WithTimeout(context.Background(), *cf.timeout)
		defer clientCancel()
		client := runnerNewClient(cf)
		// Fetch the run to resolve DecomposedFrom for shared-branch routing.
		// Best-effort: if the fetch fails, fall back to standalone branch naming.
		var decomposedFrom *uuid.UUID
		if runRow, fetchErr := client.GetRun(clientCtx, parsedRunID); fetchErr == nil {
			decomposedFrom = runRow.DecomposedFrom
		}
		_, autoErr := autoOpenPR(clientCtx, client, autoOpenPRArgs{
			WorkingDir:     *workingDir,
			RunID:          parsedRunID,
			StageID:        parsedStageID,
			GitHubRepo:     repo,
			BaseBranch:     *baseBranch,
			DecomposedFrom: decomposedFrom,
		})
		if autoErr != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk runner start: auto-PR warning: %v\n", autoErr)
		} else {
			if r := fetchRunForComment(clientCtx, client, parsedRunID); r != nil && r.RunnerKind == "local" && r.IssueContext != nil {
				if err := postOrEditStatusComment(*cf.backendURL, r.ID.String(), r.Repo, r.IssueContext.Number); err != nil && !errors.Is(err, ghcomment.ErrGhNotInstalled) {
					_, _ = fmt.Fprintf(stderr, "fishhawk runner start: comment on issue #%d: %v\n", r.IssueContext.Number, err)
				}
			}
		}
	}
	if perr == nil {
		clientCtx, clientCancel := context.WithTimeout(context.Background(), *cf.timeout)
		defer clientCancel()
		client := runnerNewClient(cf)
		if r := fetchRunForComment(clientCtx, client, parsedRunID); r != nil && r.RunnerKind == "local" && r.IssueContext != nil {
			if err := postOrEditStatusComment(*cf.backendURL, r.ID.String(), r.Repo, r.IssueContext.Number); err != nil && !errors.Is(err, ghcomment.ErrGhNotInstalled) {
				_, _ = fmt.Fprintf(stderr, "fishhawk runner start: comment on issue #%d: %v\n", r.IssueContext.Number, err)
			}
		}
	}
	return exitOK
}

// detectGitHubRepo runs `git remote get-url origin` in workingDir
// and parses the URL into owner/name form. Handles both HTTPS and
// SSH remote shapes. Empty workingDir means the CLI's CWD.
//
// Returns ErrGitRemoteParse when the URL doesn't look like a
// GitHub repo URL — callers branch on this to decide whether to
// fall back (when --no-pr is set, repo isn't strictly required).
func detectGitHubRepo(workingDir string) (string, error) {
	raw, err := gitRemoteOriginURL(workingDir)
	if err != nil {
		return "", fmt.Errorf("`git remote get-url origin`: %w", err)
	}
	owner, name, err := parseGitHubRemote(raw)
	if err != nil {
		return "", err
	}
	return owner + "/" + name, nil
}

// parseGitHubRemote turns a remote URL into (owner, name).
// Recognizes:
//
//	https://github.com/owner/name(.git)
//	https://github.com/owner/name(/)
//	git@github.com:owner/name(.git)
//	ssh://git@github.com/owner/name(.git)
//
// Returns an error for other hosts (non-github.com) so a customer
// with a self-hosted GHES instance gets a clear "this URL isn't
// github.com" failure rather than a malformed owner/name.
func parseGitHubRemote(raw string) (owner, name string, err error) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "https://github.com/"):
		s = strings.TrimPrefix(s, "https://github.com/")
	case strings.HasPrefix(s, "git@github.com:"):
		s = strings.TrimPrefix(s, "git@github.com:")
	case strings.HasPrefix(s, "ssh://git@github.com/"):
		s = strings.TrimPrefix(s, "ssh://git@github.com/")
	default:
		return "", "", fmt.Errorf("remote %q is not a github.com URL", raw)
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("remote %q is not owner/name", raw)
	}
	return parts[0], parts[1], nil
}
