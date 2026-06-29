package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runCampaign dispatches `fishhawk campaign <subcommand>`. It mirrors
// runDeploy's shape and drives the E25.4 campaign REST surface from the
// terminal (E25.9 / #1448): `start` mints a campaign from an epic ref,
// `status` renders the rollup + next_action + per-issue run grid, `list`
// pages campaigns, and `resume` hands a paused campaign back to the
// auto-driver after a human owned a gate.
func runCampaign(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk campaign: subcommand required (start|status|list|resume)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return campaignStart(rest, stdout, stderr)
	case "status":
		return campaignStatus(rest, stdout, stderr)
	case "list":
		return campaignList(rest, stdout, stderr)
	case "resume":
		return campaignResume(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk campaign: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// validPausePolicies is the set the backend accepts for --pause-policy.
// Validated CLI-side before the round-trip so a typo fails in ms with a
// precise message instead of a generic 400.
var validPausePolicies = map[string]bool{
	"pause_campaign": true,
	"pause_item":     true,
}

// campaignStart implements `fishhawk campaign start --repo R --epic E
// [--pause-policy P] [--output text|json]`. It normalizes --epic to the
// canonical issue:N form the API expects, validates --pause-policy
// locally, and POSTs to /v0/campaigns.
func campaignStart(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk campaign start"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "owner/name of the repo (required)")
	epic := fs.String("epic", "", "epic issue ref: issue:N, #N, N, or .../issues/N URL (required)")
	pausePolicy := fs.String("pause-policy", "",
		"auto-driver gate pause behavior: pause_campaign | pause_item; empty uses the backend default")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if *repo == "" || *epic == "" {
		_, _ = fmt.Fprintf(stderr, "%s: --repo and --epic are required\n", name)
		return exitUsage
	}
	if *pausePolicy != "" && !validPausePolicies[*pausePolicy] {
		_, _ = fmt.Fprintf(stderr,
			"%s: invalid --pause-policy %q (want pause_campaign|pause_item)\n", name, *pausePolicy)
		return exitUsage
	}

	// Normalize --epic to the canonical issue:N form. resolveIssueRef
	// accepts #N, N, and .../issues/N URL forms but returns the int; a
	// bare "issue:N" string is not a number, so strip the prefix first.
	epicRaw := strings.TrimPrefix(strings.TrimSpace(*epic), "issue:")
	epicNum, err := resolveIssueRef(epicRaw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: --epic: %v\n", name, err)
		return exitUsage
	}
	if epicNum == 0 {
		_, _ = fmt.Fprintf(stderr, "%s: --epic must be a non-zero issue ref\n", name)
		return exitUsage
	}
	epicRef := fmt.Sprintf("issue:%d", epicNum)

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	camp, err := newClient(cf).CreateCampaign(ctx, httpclient.CreateCampaignInput{
		Repo:        *repo,
		EpicRef:     epicRef,
		PausePolicy: *pausePolicy,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(camp); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		printCampaign(stdout, camp)
	}
	return exitOK
}

// campaignStatus implements `fishhawk campaign status <campaign-id>
// [--output text|json]`. It renders the campaign block, the next_action
// line, and a per-issue run grid (one line per item).
func campaignStatus(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk campaign status"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	positionals, err := parseIntermixed(fs, args)
	if err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintf(stderr, "%s: <campaign-id> required\n", name)
		return exitUsage
	}
	campaignID, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %q is not a UUID: %v\n", name, positionals[0], err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	st, err := newClient(cf).GetCampaignStatus(ctx, campaignID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(st); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		printCampaignStatus(stdout, st)
	}
	return exitOK
}

// campaignList implements `fishhawk campaign list [--repo R] [--state S]
// [--limit N] [--cursor X]`. It mirrors runList's plain tabular print
// (no --output flag).
func campaignList(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk campaign list"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "filter by repo (owner/name)")
	state := fs.String("state", "", "filter by state (pending|running|succeeded|failed|cancelled)")
	limit := fs.Int("limit", 0, "max items per page (default 50, max 200)")
	cursor := fs.String("cursor", "", "pagination cursor from a prior list response")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	res, err := newClient(cf).ListCampaigns(ctx, httpclient.ListCampaignsFilter{
		Repo:   *repo,
		State:  *state,
		Limit:  *limit,
		Cursor: *cursor,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}
	if len(res.Items) == 0 {
		_, _ = fmt.Fprintln(stdout, "(no campaigns)")
		return exitOK
	}
	for _, camp := range res.Items {
		_, _ = fmt.Fprintf(stdout, "%s  %-30s  %-15s  %-10s  %s\n",
			camp.ID, camp.Repo, camp.EpicRef, camp.State, camp.CreatedAt.Format(time.RFC3339))
	}
	if res.NextCursor != "" {
		_, _ = fmt.Fprintf(stdout, "\nMore: --cursor %s\n", res.NextCursor)
	}
	return exitOK
}

// campaignResume implements `fishhawk campaign resume <campaign-id>
// [--output text|json]`. It hands a paused campaign back to the
// auto-driver. Server-side preconditions (409 campaign_not_paused, 403
// insufficient_scope) surface verbatim via exitOnAPIError.
func campaignResume(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk campaign resume"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	positionals, err := parseIntermixed(fs, args)
	if err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintf(stderr, "%s: <campaign-id> required\n", name)
		return exitUsage
	}
	campaignID, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %q is not a UUID: %v\n", name, positionals[0], err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	camp, err := newClient(cf).ResumeCampaign(ctx, campaignID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(camp); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		printCampaign(stdout, camp)
	}
	return exitOK
}

// printCampaign renders a Campaign as a small block of human-readable
// lines, following printRun's field-alignment shape.
func printCampaign(w io.Writer, c *httpclient.Campaign) {
	_, _ = fmt.Fprintf(w, "id:             %s\n", c.ID)
	_, _ = fmt.Fprintf(w, "repo:           %s\n", c.Repo)
	_, _ = fmt.Fprintf(w, "epic_ref:       %s\n", c.EpicRef)
	_, _ = fmt.Fprintf(w, "state:          %s\n", c.State)
	_, _ = fmt.Fprintf(w, "pause_policy:   %s\n", c.PausePolicy)
	_, _ = fmt.Fprintf(w, "created_at:     %s\n", c.CreatedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "updated_at:     %s\n", c.UpdatedAt.Format(time.RFC3339))
}

// printCampaignStatus renders a CampaignStatus: the campaign block, the
// distilled next_action line, and a per-issue run grid (one line per
// item showing issue_ref, state, and run_id-or-'-').
func printCampaignStatus(w io.Writer, st *httpclient.CampaignStatus) {
	printCampaign(w, &st.Campaign)

	na := st.NextAction
	line := "next_action:    " + na.Action
	if na.IssueRef != "" {
		line += " " + na.IssueRef
	}
	if na.Detail != "" {
		line += " — " + na.Detail
	}
	_, _ = fmt.Fprintln(w, line)

	_, _ = fmt.Fprintln(w, "items:")
	for _, item := range st.Items {
		runID := "-"
		if item.RunID != nil {
			runID = item.RunID.String()
		}
		_, _ = fmt.Fprintf(w, "  %-15s  %-10s  %s\n", item.IssueRef, item.State, runID)
	}
}
