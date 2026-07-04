package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/ghcomment"
	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runRun dispatches to `fishhawk run <subcommand>`. Each
// subcommand has its own flag set; common flags (backend URL,
// token) live in newClient and are consumed by every subcommand.
func runRun(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk run: subcommand required (start|status|list|cancel|open|retry|watch|auto-decide)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return runStart(rest, stdout, stderr)
	case "status":
		return runStatus(rest, stdout, stderr)
	case "list":
		return runList(rest, stdout, stderr)
	case "cancel":
		return runCancel(rest, stdout, stderr)
	case "open":
		return runOpen(rest, stdout, stderr)
	case "retry":
		return runRetry(rest, stdout, stderr)
	case "watch":
		return runWatch(rest, stdout, stderr)
	case "auto-decide":
		return runAutoDecide(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk run: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// commonFlags binds --backend-url and --token onto fs and returns
// pointers consumed by newClient. Centralized so every subcommand
// has the same flags in the same order.
type commonFlags struct {
	backendURL *string
	token      *string
	timeout    *time.Duration
}

func bindCommonFlags(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		backendURL: fs.String("backend-url",
			envOr("FISHHAWK_BACKEND_URL", "http://localhost:8080"),
			"Fishhawk backend URL"),
		token: fs.String("token",
			envOr("FISHHAWK_TOKEN", ""),
			"Bearer token; may be empty for dev backends with stubbed auth"),
		timeout: fs.Duration("timeout", 60*time.Second,
			"per-request timeout"),
	}
}

func newClient(cf commonFlags) *httpclient.Client {
	c := httpclient.New(*cf.backendURL, *cf.token)
	c.HTTP.Timeout = *cf.timeout
	return c
}

// runStart implements `fishhawk run start`.
//
// Workflow-spec discovery (#411): when --spec-file is set the CLI
// reads exactly that file; otherwise it walks up from --working-dir
// looking for `.fishhawk/workflows.yaml`, stopping at the .git
// boundary. A discovered spec is pre-parsed locally so a YAML typo
// fails the verb in ms instead of round-tripping to the backend,
// and its bytes ride along inline so the backend can create one
// Stage row per stage definition. The user-supplied --workflow-sha
// (if any) overrides the computed blob SHA — useful when minting a
// retrospective run against a known commit. When no spec is found
// and --workflow-sha was not supplied, the verb errors with both
// remediation options.
//
// Issue context (#415): when --issue is set (or trigger-ref is in
// `issue:N` shape), the CLI shells to `gh issue view` and ships
// the title/body/url/number inline so the backend's prompt
// builder reads the cached payload instead of needing an
// installation_id. Best-effort: a missing or unauthed gh emits a
// warning to stderr and the run proceeds without the cache.
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "owner/name of the repo (required)")
	workflowID := fs.String("workflow", "", "workflow ID matching .fishhawk/workflows.yaml (required)")
	workflowSHA := fs.String("workflow-sha", "",
		"git blob SHA of .fishhawk/workflows.yaml; auto-computed from the discovered spec when omitted")
	triggerRef := fs.String("trigger-ref", "", "optional trigger reference (e.g. issue:1247)")
	runnerKind := fs.String("runner-kind", "",
		"execution backend tag (ADR-022): github_actions | local; empty uses the backend's default")
	workingDir := fs.String("working-dir", ".",
		"directory to search for .fishhawk/workflows.yaml (walks up to the .git boundary)")
	specFile := fs.String("spec-file", "",
		"explicit path to a workflow spec file; overrides auto-discovery")
	issueArg := fs.String("issue", "",
		"GitHub issue number, #N, or .../issues/N URL; CLI fetches via `gh` and ships inline")
	overrideBudget := fs.Bool("override-budget", false,
		"force the run past a blocking periodic cost budget that is over its limit for the current period (#688)")
	upstreamRunID := fs.String("upstream-run-id", "",
		"UUID of the upstream feature_change run whose ci_green/review_merged a deploy-only release run's required_upstream pre-flight gate evaluates (E23.11/#1417); distinct from parent_run_id")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *repo == "" || *workflowID == "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk run start: --repo and --workflow are required")
		fs.Usage()
		return exitUsage
	}
	if *upstreamRunID != "" {
		if _, err := uuid.Parse(*upstreamRunID); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk run start: --upstream-run-id %q is not a valid UUID\n", *upstreamRunID)
			return exitUsage
		}
	}

	// Parse the explicit --issue argument up front so a typo
	// surfaces before any backend round-trip.
	issueNumber, err := resolveIssueRef(*issueArg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run start: %v\n", err)
		return exitUsage
	}
	// When --issue isn't set but trigger-ref is `issue:N`, derive
	// the number from the trigger ref. Saves the operator typing
	// the number twice.
	if issueNumber == 0 {
		issueNumber = inferIssueNumberFromTriggerRef(*triggerRef)
	}

	// Resolve the workflow spec. Errors here come from explicit
	// --spec-file misses or unreadable candidate files; "no spec
	// found in the walk" is signalled by a nil return without an
	// error.
	found, err := discoverSpec(*workingDir, *specFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run start: %v\n", err)
		return exitFailure
	}

	// Compose the effective SHA. Flag wins over auto-computed (an
	// override hook for minting historic runs); otherwise the
	// discovered file's blob SHA travels with the bytes.
	effectiveSHA := *workflowSHA
	var specBytes []byte
	if found != nil {
		if effectiveSHA == "" {
			effectiveSHA = found.BlobSHA
		}
		specBytes = found.Contents
		// Pre-parse so a YAML typo is a fast local failure
		// instead of a backend round-trip. The backend also
		// parses — the CLI check is purely UX.
		if perr := cliSpecValidate(specBytes); perr != nil {
			_, _ = fmt.Fprintf(stderr,
				"fishhawk run start: %s: %v\n", found.Path, perr)
			return exitFailure
		}
	}
	if effectiveSHA == "" {
		_, _ = fmt.Fprintln(stderr,
			"fishhawk run start: workflow spec not found in --working-dir (and no --workflow-sha override). Pass --spec-file or run from a checkout that has .fishhawk/workflows.yaml.")
		return exitFailure
	}

	// Default trigger_source to "cli"; an --issue argument (or an
	// issue:N trigger-ref) bumps it to "github_issue" so the
	// backend accepts the optional issue_context payload and
	// the prompt-builder picks the issue-driven template.
	triggerSource := "cli"
	if issueNumber > 0 {
		triggerSource = "github_issue"
		// Normalize trigger_ref to the canonical issue:N form
		// when the operator only passed --issue, so threading +
		// audit surfaces (#216) keep working.
		if *triggerRef == "" {
			derived := fmt.Sprintf("issue:%d", issueNumber)
			triggerRef = &derived
		}
	}

	in := httpclient.CreateRunInput{
		Repo:           *repo,
		WorkflowID:     *workflowID,
		WorkflowSHA:    effectiveSHA,
		TriggerSource:  triggerSource,
		RunnerKind:     *runnerKind,
		WorkflowSpec:   string(specBytes),
		BudgetOverride: *overrideBudget,
	}
	if *triggerRef != "" {
		in.TriggerRef = triggerRef
	}
	if *upstreamRunID != "" {
		in.UpstreamRunID = upstreamRunID
	}

	// Fetch the issue locally via gh and bundle the payload.
	// Best-effort: a missing or unauthed gh emits a warning and
	// the run proceeds without the cache (degraded prompt =
	// pre-#415 shape).
	if issueNumber > 0 {
		ic, ferr := fetchIssueViaGh(*repo, issueNumber)
		switch {
		case ferr == nil:
			in.IssueContext = ic
		case errors.Is(ferr, ErrGhNotInstalled):
			_, _ = fmt.Fprintln(stderr,
				"fishhawk run start: gh CLI not on PATH; proceeding without inline issue context. Install https://cli.github.com for the full prompt.")
		default:
			_, _ = fmt.Fprintf(stderr,
				"fishhawk run start: warning: %v — proceeding without inline issue context\n", ferr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	r, err := newClient(cf).StartRun(ctx, in)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run start: %v\n", err)
		return exitFailure
	}
	printRun(stdout, r)

	// #428: for local-runner runs triggered by a GitHub issue, post
	// or edit the sticky status comment. Best-effort; nil-safe when
	// not applicable.
	if r.RunnerKind == "local" && r.IssueContext != nil {
		if err := postOrEditStatusComment(*cf.backendURL, r.ID.String(), r.Repo, r.IssueContext.Number); err != nil && !errors.Is(err, ghcomment.ErrGhNotInstalled) {
			_, _ = fmt.Fprintf(stderr, "fishhawk: comment on issue #%d failed: %v\n", r.IssueContext.Number, err)
		}
	}
	return exitOK
}

// runStatus implements `fishhawk run status <run-id>`.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	positionals, err := parseIntermixed(fs, args)
	if err != nil {
		return exitUsage
	}
	switch *outputFmt {
	case "text", "json":
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk run status: invalid --output %q (want text|json)\n", *outputFmt)
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk run status: <run-id> required")
		return exitUsage
	}
	id, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run status: %q is not a UUID: %v\n", positionals[0], err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	r, err := newClient(cf).GetRun(ctx, id)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run status: %v\n", err)
		return exitOnAPIError(err)
	}
	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(r); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk run status: encode: %v\n", err)
			return exitFailure
		}
	default:
		printRun(stdout, r)
	}
	return exitOK
}

// runList implements `fishhawk run list`.
func runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "filter by repo (owner/name)")
	workflow := fs.String("workflow", "", "filter by workflow ID")
	state := fs.String("state", "", "filter by state (pending|running|succeeded|failed|cancelled)")
	limit := fs.Int("limit", 0, "max items per page (default 50, max 200)")
	cursor := fs.String("cursor", "", "pagination cursor from a prior list response")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	res, err := newClient(cf).ListRuns(ctx, httpclient.ListRunsFilter{
		Repo:       *repo,
		WorkflowID: *workflow,
		State:      *state,
		Limit:      *limit,
		Cursor:     *cursor,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run list: %v\n", err)
		return exitOnAPIError(err)
	}
	if len(res.Items) == 0 {
		_, _ = fmt.Fprintln(stdout, "(no runs)")
		return exitOK
	}
	for _, r := range res.Items {
		_, _ = fmt.Fprintf(stdout, "%s  %-30s  %-15s  %-10s  %s\n",
			r.ID, r.Repo, r.WorkflowID, r.State, r.CreatedAt.Format(time.RFC3339))
	}
	if res.NextCursor != "" {
		_, _ = fmt.Fprintf(stdout, "\nMore: --cursor %s\n", res.NextCursor)
	}
	return exitOK
}

// runCancel implements `fishhawk run cancel <run-id>`.
func runCancel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk run cancel: <run-id> required")
		return exitUsage
	}
	id, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run cancel: %q is not a UUID: %v\n", fs.Arg(0), err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	r, err := newClient(cf).CancelRun(ctx, id)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run cancel: %v\n", err)
		return exitOnAPIError(err)
	}
	printRun(stdout, r)

	// #428: sticky status comment for local-runner issue-triggered runs.
	if r.RunnerKind == "local" && r.IssueContext != nil {
		if err := postOrEditStatusComment(*cf.backendURL, r.ID.String(), r.Repo, r.IssueContext.Number); err != nil && !errors.Is(err, ghcomment.ErrGhNotInstalled) {
			_, _ = fmt.Fprintf(stderr, "fishhawk: comment on issue #%d failed: %v\n", r.IssueContext.Number, err)
		}
	}
	return exitOK
}

// runOpen builds the canonical UI URL for the run and shells out
// to the OS-appropriate "open this URL" tool. We don't reach the
// backend at all — opening the UI is a local-only operation.
func runOpen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	printOnly := fs.Bool("print-url", false, "print the URL instead of launching the browser")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk run open: <run-id> required")
		return exitUsage
	}
	id, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run open: %q is not a UUID: %v\n", fs.Arg(0), err)
		return exitUsage
	}
	url := strings.TrimRight(*cf.backendURL, "/") + "/runs/" + id.String()
	if *printOnly {
		_, _ = fmt.Fprintln(stdout, url)
		return exitOK
	}
	if err := openBrowser(url); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run open: %v (use --print-url to bypass)\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintln(stdout, url)
	return exitOK
}

// runRetry implements `fishhawk run retry <stage-id> [--output text|json]`.
// Takes a STAGE id, not a run id: retry is stage-scoped per
// docs/MVP_SPEC §6 (only failed stages are retryable, and the
// retryable failure categories differ). The CLI doesn't try to
// resolve which stage the user means — they pass the failed stage's
// id directly. Server-side rejections (non-failed stage,
// non-retryable failure category) come back as *APIError with the
// envelope's code (e.g. `retry_not_applicable`); the CLI surfaces
// the API error message verbatim.
func runRetry(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run retry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run retry: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk run retry: <stage-id> required")
		return exitUsage
	}
	stageID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run retry: %q is not a UUID: %v\n", fs.Arg(0), err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	stage, err := newClient(cf).RetryStage(ctx, stageID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run retry: %v\n", err)
		return exitOnAPIError(err)
	}
	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(stage); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk run retry: encode: %v\n", err)
			return exitFailure
		}
	default:
		printStage(stdout, stage)
	}
	return exitOK
}

// openBrowser is a small platform shim. macOS / Linux / Windows
// each have a different "open this URL" command; the CLI honors
// $BROWSER first when set so users on headless systems can
// override.
var openBrowser = func(url string) error {
	if cmd := envOr("BROWSER", ""); cmd != "" {
		return exec.Command(cmd, url).Start()
	}
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default: // linux, freebsd, etc.
		return exec.Command("xdg-open", url).Start()
	}
}

// printRun writes a Run as a small block of human-readable lines
// to w. Mirrors the OpenAPI Run shape so a user reading the CLI's
// output and the UI's run-detail page see the same fields.
func printRun(w io.Writer, r *httpclient.Run) {
	_, _ = fmt.Fprintf(w, "id:             %s\n", r.ID)
	_, _ = fmt.Fprintf(w, "repo:           %s\n", r.Repo)
	_, _ = fmt.Fprintf(w, "workflow_id:    %s\n", r.WorkflowID)
	_, _ = fmt.Fprintf(w, "workflow_sha:   %s\n", r.WorkflowSHA)
	_, _ = fmt.Fprintf(w, "trigger_source: %s\n", r.TriggerSource)
	if r.TriggerRef != nil {
		_, _ = fmt.Fprintf(w, "trigger_ref:    %s\n", *r.TriggerRef)
	}
	_, _ = fmt.Fprintf(w, "state:          %s\n", r.State)
	if r.RunnerKind != "" {
		_, _ = fmt.Fprintf(w, "runner_kind:    %s\n", r.RunnerKind)
	}
	if r.UpstreamRunID != nil {
		_, _ = fmt.Fprintf(w, "upstream_run_id: %s\n", r.UpstreamRunID)
	}
	_, _ = fmt.Fprintf(w, "created_at:     %s\n", r.CreatedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "updated_at:     %s\n", r.UpdatedAt.Format(time.RFC3339))
}

// exitOnAPIError maps an *httpclient.APIError to the CLI's exit
// code. 4xx errors map to exitFailure (caller did something
// wrong); 5xx also exitFailure but operators can switch on the
// printed status. Non-API errors (network, parse) → exitFailure.
func exitOnAPIError(err error) int {
	var apiErr *httpclient.APIError
	if errors.As(err, &apiErr) {
		return exitFailure
	}
	return exitFailure
}
