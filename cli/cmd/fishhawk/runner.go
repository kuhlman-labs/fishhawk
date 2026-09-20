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
	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
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

// Forge ids the CLI recognises (E45.46 / #3463). They are the
// fishhawk-runner's `--forge` values and the backend's Run.forge
// values verbatim.
const (
	forgeGitHub = "github"
	forgeGitLab = "gitlab"
)

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
// (backend URL, token, working dir, repo, forge), composes the
// fishhawk-runner subprocess argv, spawns it, pipes its stdout and
// stderr through, and exits with the runner's exit code. Test seams
// (`runnerStartCommand`, `runnerBinaryLookPath`, `gitRemoteOriginURL`,
// `runnerNewClient`) let unit tests assert on the constructed argv
// without spawning a real subprocess.
//
// Per ADR-022's addendum (#388): local-runner runs carry
// runner_kind=local at the backend; the operator-side write tools
// minted the run with that tag via `fishhawk run start
// --runner-kind local` upstream. This verb just invokes the
// runner against an already-minted run.
//
// Forge target (E45.46 / #3463). A run row carries its forge, and a
// gitlab run must be spawned with `--forge gitlab --gitlab-base-url
// <url>` or the runner targets api.github.com for the push + MR open.
// So whenever ANY of --forge, --github-repo, or (for a gitlab target)
// --gitlab-base-url is omitted, the verb reads the run row FIRST via
// the single-run GET /v0/runs/{id} — the only route the backend
// serves forge_base_url on — and resolves each omitted value from it.
// The read is FAIL-CLOSED, symmetric with the MCP spawn producers:
// with --forge omitted an unreadable row REFUSES to spawn rather than
// defaulting to github with a warning, because a gitlab run spawned as
// github is the silent wrong-forge class this exists to close.
// Explicit flags always win over the row.
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
		"repo slug (owner/name, or the GitLab path_with_namespace); defaults to the run row's repo for a gitlab run, else auto-detected from `git remote get-url origin`")
	forgeFlag := fs.String("forge", "",
		"github | gitlab; omitted derives from the run row")
	gitlabBaseURL := fs.String("gitlab-base-url", "",
		"GitLab instance root; omitted derives from the run row's forge_base_url")
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
	switch *forgeFlag {
	case "", forgeGitHub, forgeGitLab:
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk runner start: --forge %q is not one of github, gitlab\n", *forgeFlag)
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

	// Pre-spawn run-row read (E45.46 / #3463). Needed whenever a
	// forge-dependent value is omitted: the forge itself, the repo
	// (the row's path_with_namespace is the only sane default for a
	// gitlab run — origin auto-detect is github.com-only), or the
	// gitlab base URL. Runs BEFORE the origin auto-detect and BEFORE
	// the argv build; all flags explicit → zero network calls, as
	// before.
	forge := *forgeFlag
	baseURL := *gitlabBaseURL
	repo := *githubRepo
	needRow := forge == "" || repo == "" || (forge == forgeGitLab && baseURL == "")
	var row *httpclient.Run
	if needRow {
		parsedRunID, perr := uuid.Parse(*runID)
		var readErr error
		if perr != nil {
			readErr = fmt.Errorf("--run-id %q is not a UUID: %w", *runID, perr)
		} else {
			rowCtx, rowCancel := context.WithTimeout(context.Background(), *cf.timeout)
			row, readErr = runnerNewClient(cf).GetRun(rowCtx, parsedRunID)
			rowCancel()
		}
		if readErr != nil {
			row = nil
			switch forge {
			case "":
				// Fail closed: never a github default with a warning.
				_, _ = fmt.Fprintf(stderr,
					"fishhawk runner start: could not read run %s to resolve its forge (%v); not spawning (a gitlab run spawned as github would target api.github.com). Pass --forge explicitly, or fix backend reachability (--backend-url / FISHHAWK_BACKEND_URL)\n",
					*runID, readErr)
				return exitFailure
			case forgeGitLab:
				// The forge is known but a gitlab spawn still needs
				// what the row would have supplied; name each
				// missing flag.
				var missing []string
				if baseURL == "" {
					missing = append(missing, "--gitlab-base-url")
				}
				if repo == "" {
					missing = append(missing, "--github-repo")
				}
				_, _ = fmt.Fprintf(stderr,
					"fishhawk runner start: could not read run %s (%v) and --forge gitlab needs %s; not spawning. Pass the flag(s) explicitly, or fix backend reachability (--backend-url / FISHHAWK_BACKEND_URL)\n",
					*runID, readErr, strings.Join(missing, " and "))
				return exitFailure
			default:
				// --forge github explicit with only --github-repo
				// omitted: the forge is known and the run-row repo
				// default is a convenience the github path never
				// had, so fall through to today's origin auto-detect.
			}
		}
	}
	if forge == "" && row != nil {
		switch row.Forge {
		case "", forgeGitHub:
			forge = forgeGitHub
		case forgeGitLab:
			forge = forgeGitLab
		default:
			// Fail closed: never fall through to a github argv for a
			// forge this CLI doesn't know (E45.59 / #3505), mirroring
			// resolveRunForgeTarget's unknown-forge arm in
			// backend/internal/mcpserver/run_stage.go.
			_, _ = fmt.Fprintf(stderr,
				"fishhawk runner start: run %s carries unknown forge %q; not spawning (this fishhawk CLI knows github and gitlab — rebuild it against the backend that minted the run, or pass --forge explicitly)\n",
				*runID, row.Forge)
			return exitFailure
		}
	}
	if forge == "" {
		forge = forgeGitHub
	}
	if forge == forgeGitLab {
		if baseURL == "" && row != nil {
			baseURL = row.ForgeBaseURL
		}
		if baseURL == "" {
			// Refuse BEFORE any spawn: the runner with --forge gitlab
			// and no base URL would refuse too, but naming both
			// remedies here saves the round trip.
			_, _ = fmt.Fprintf(stderr,
				"fishhawk runner start: run %s is a gitlab run but no GitLab base URL is known; not spawning. Pass --gitlab-base-url, or set FISHHAWKD_GITLAB_BASE_URL on fishhawkd / re-register the installation with --forge-base-url so the run row carries forge_base_url\n",
				*runID)
			return exitFailure
		}
		if repo == "" && row != nil {
			// The run row's project path is the only sane default
			// on gitlab: detectGitHubRepo parses github.com URLs
			// only and is skipped.
			repo = row.Repo
		}
	}

	// Resolve the GitHub repo. Flag wins; otherwise auto-detect
	// from `git remote get-url origin`. Auto-detect failure is a
	// soft failure — the runner can still proceed when --no-pr is
	// set (no push, no PR, no repo lookup needed). For PR-shaped
	// runs without --github-repo and no detectable origin, the
	// runner will surface its own error.
	if repo == "" && forge != forgeGitLab {
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
	// A gitlab target rides on the runner's own --forge /
	// --gitlab-base-url flags (ADR-058 / E45.5); github emits
	// nothing new so the github argv is byte-identical to before.
	if forge == forgeGitLab {
		argv = append(argv, "--forge", forgeGitLab, "--gitlab-base-url", baseURL)
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
	// The sticky status comment is posted via `gh` against
	// github.com, so a gitlab run skips both comment blocks (the
	// auto-PR path is github-only too — the runner opened the MR).
	if forge == forgeGitLab {
		return exitOK
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
