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

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runRun dispatches to `fishhawk run <subcommand>`. Each
// subcommand has its own flag set; common flags (backend URL,
// token) live in newClient and are consumed by every subcommand.
func runRun(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk run: subcommand required (start|status|list|cancel|open)`)
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
func runStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "owner/name of the repo (required)")
	workflowID := fs.String("workflow", "", "workflow ID matching .fishhawk/workflows.yaml (required)")
	workflowSHA := fs.String("workflow-sha", "", "git SHA of .fishhawk/workflows.yaml (required)")
	triggerRef := fs.String("trigger-ref", "", "optional trigger reference (e.g. issue:1247)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *repo == "" || *workflowID == "" || *workflowSHA == "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk run start: --repo, --workflow, and --workflow-sha are required")
		fs.Usage()
		return exitUsage
	}

	in := httpclient.CreateRunInput{
		Repo:          *repo,
		WorkflowID:    *workflowID,
		WorkflowSHA:   *workflowSHA,
		TriggerSource: "cli",
	}
	if *triggerRef != "" {
		in.TriggerRef = triggerRef
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	r, err := newClient(cf).StartRun(ctx, in)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run start: %v\n", err)
		return exitFailure
	}
	printRun(stdout, r)
	return exitOK
}

// runStatus implements `fishhawk run status <run-id>`.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch *outputFmt {
	case "text", "json":
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk run status: invalid --output %q (want text|json)\n", *outputFmt)
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk run status: <run-id> required")
		return exitUsage
	}
	id, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run status: %q is not a UUID: %v\n", fs.Arg(0), err)
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
