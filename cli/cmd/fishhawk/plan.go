package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runPlan dispatches `fishhawk plan <subcommand>`. Closes the
// SPA-only gap on plan review per ADR-019 / #320: every action the
// SPA surfaces must also be reachable from the surfaces developers
// already use, including the terminal.
func runPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk plan: subcommand required (approve)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "approve":
		return planApprove(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk plan: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// planApprove implements `fishhawk plan approve <run-id> [--reason ...] [--output text|json]`.
// Resolves the plan stage from the run id (the operator-facing
// identifier; the SPA exposes stages only as nested sub-routes), then
// POSTs the approval. Mirrors the slash-command flow's resolution
// logic so the same "no plan stage awaiting approval" condition
// surfaces with the same error.
func planApprove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk plan approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	reason := fs.String("reason", "", "optional approval comment recorded on the approval row")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk plan approve: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk plan approve: <run-id> required")
		return exitUsage
	}
	runID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk plan approve: %q is not a UUID: %v\n", fs.Arg(0), err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	client := newClient(cf)

	stages, err := client.ListRunStages(ctx, runID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk plan approve: list stages: %v\n", err)
		return exitOnAPIError(err)
	}
	planStage := findAwaitingApprovalPlanStage(stages.Items)
	if planStage == nil {
		_, _ = fmt.Fprintf(stderr,
			"fishhawk plan approve: run %s has no plan stage awaiting approval (check `fishhawk run status %s`)\n",
			runID, runID)
		return exitFailure
	}

	stage, err := client.SubmitApproval(ctx, planStage.ID, httpclient.SubmitApprovalInput{
		Decision: httpclient.ApprovalApprove,
		Comment:  *reason,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk plan approve: %v\n", err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(stage); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk plan approve: encode: %v\n", err)
			return exitFailure
		}
	default:
		printStage(stdout, stage)
	}
	return exitOK
}

// findAwaitingApprovalPlanStage walks the stage list (sequence
// ascending) and returns the first plan stage in awaiting_approval.
// Returns nil when no such stage exists — the caller surfaces a
// help message naming the run.
func findAwaitingApprovalPlanStage(stages []httpclient.Stage) *httpclient.Stage {
	for i := range stages {
		if stages[i].Type == "plan" && stages[i].State == "awaiting_approval" {
			return &stages[i]
		}
	}
	return nil
}

// validateOutputFormat enforces the text | json contract for
// --output. Centralized so future subcommands can reuse the check
// without diverging on the error wording.
func validateOutputFormat(v string) error {
	switch v {
	case "text", "json":
		return nil
	}
	return errors.New("invalid --output (want text|json)")
}

// printStage renders a Stage as a small block of human-readable
// lines. Mirrors printRun's shape so the SPA's stage-detail and the
// CLI's stage echo share field naming.
func printStage(w io.Writer, s *httpclient.Stage) {
	_, _ = fmt.Fprintf(w, "id:             %s\n", s.ID)
	_, _ = fmt.Fprintf(w, "run_id:         %s\n", s.RunID)
	_, _ = fmt.Fprintf(w, "sequence:       %d\n", s.Sequence)
	_, _ = fmt.Fprintf(w, "type:           %s\n", s.Type)
	_, _ = fmt.Fprintf(w, "executor:       %s:%s\n", s.Executor.Kind, s.Executor.Ref)
	_, _ = fmt.Fprintf(w, "state:          %s\n", s.State)
	if s.FailureCategory != nil {
		_, _ = fmt.Fprintf(w, "failure_cat:    %s\n", *s.FailureCategory)
	}
	if s.FailureReason != nil {
		_, _ = fmt.Fprintf(w, "failure_reason: %s\n", *s.FailureReason)
	}
}
