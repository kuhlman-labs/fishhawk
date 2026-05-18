package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/ghcomment"
	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runPlan dispatches `fishhawk plan <subcommand>`. Closes the
// SPA-only gap on plan review per ADR-019 / #320: every action the
// SPA surfaces must also be reachable from the surfaces developers
// already use, including the terminal.
func runPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk plan: subcommand required (approve|reject)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "approve":
		return planApprove(rest, stdout, stderr)
	case "reject":
		return planReject(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk plan: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// planApprove implements `fishhawk plan approve <run-id> [--reason ...] [--output text|json]`.
// Resolves the plan stage from the run id and POSTs an approve
// decision.
func planApprove(args []string, stdout, stderr io.Writer) int {
	return planDecision("fishhawk plan approve", httpclient.ApprovalApprove,
		args, stdout, stderr)
}

// planReject implements `fishhawk plan reject <run-id> [--reason ...] [--output text|json]`.
// Same flow as planApprove but submits a reject decision, which the
// state machine resolves as a category-D stage failure.
//
// The CLI emits a soft warning (to stderr, doesn't change the exit
// code) when --reason is omitted: rejection without a recorded
// rationale is allowed but the audit row would have an empty
// comment, which is unhelpful for the requester reading back what
// changed.
func planReject(args []string, stdout, stderr io.Writer) int {
	return planDecision("fishhawk plan reject", httpclient.ApprovalReject,
		args, stdout, stderr)
}

// planDecision is the shared body of `plan approve` / `plan reject`.
// It owns flag parsing, run-id validation, plan-stage resolution,
// the approvals POST, and output formatting. The two verbs differ
// only in the decision passed to SubmitApproval and the soft-warning
// behavior reject opts into for missing --reason.
func planDecision(name string, decision httpclient.ApprovalDecision, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	reason := fs.String("reason", "", "optional comment recorded on the approval row")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintf(stderr, "%s: <run-id> required\n", name)
		return exitUsage
	}
	runID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %q is not a UUID: %v\n", name, fs.Arg(0), err)
		return exitUsage
	}
	if decision == httpclient.ApprovalReject && *reason == "" {
		// Soft warning. Reject without a reason is wire-legal but
		// produces an audit row whose comment is empty, leaving the
		// requester guessing why the plan got blocked. Don't fail
		// the command — operators sometimes legitimately want a
		// silent reject (e.g. scripted clean-up) — but make the
		// loss visible.
		_, _ = fmt.Fprintf(stderr,
			"%s: warning: --reason not provided; the approval row will record an empty comment\n", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	client := newClient(cf)

	planStage, exitCode := resolvePlanStage(ctx, client, name, runID, stderr)
	if planStage == nil {
		return exitCode
	}

	stage, err := client.SubmitApproval(ctx, planStage.ID, httpclient.SubmitApprovalInput{
		Decision: decision,
		Comment:  *reason,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(stage); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		printStage(stdout, stage)
	}

	// #416: comment on the triggering issue when this is a local-
	// runner run. SubmitApproval returns the stage; we need the
	// run row to read RunnerKind + IssueContext, so an extra
	// GetRun is unavoidable. Best-effort — failures don't change
	// the verb's exit code.
	if r := fetchRunForComment(ctx, client, runID); r != nil {
		gcr := toGhCommentRun(r, *cf.backendURL)
		handle := resolveGitHubHandle()
		var body string
		switch decision {
		case httpclient.ApprovalApprove:
			body = ghcomment.RenderPlanApproved(gcr, handle)
		case httpclient.ApprovalReject:
			body = ghcomment.RenderPlanRejected(gcr, handle, *reason)
		}
		maybePostLocalComment(stderr, r, body)
	}
	return exitOK
}

// resolvePlanStage finds the plan stage that's awaiting approval on
// the given run. Returns (stage, exitOK) on success; (nil, exitCode)
// when no awaiting-approval plan stage exists or the list call
// failed. Centralized so plan approve / plan reject (and any future
// plan-* verbs) share the same lookup + error wording.
func resolvePlanStage(ctx context.Context, client *httpclient.Client, name string, runID uuid.UUID, stderr io.Writer) (*httpclient.Stage, int) {
	stages, err := client.ListRunStages(ctx, runID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: list stages: %v\n", name, err)
		return nil, exitOnAPIError(err)
	}
	planStage := findAwaitingApprovalPlanStage(stages.Items)
	if planStage == nil {
		_, _ = fmt.Fprintf(stderr,
			"%s: run %s has no plan stage awaiting approval (check `fishhawk run status %s`)\n",
			name, runID, runID)
		return nil, exitFailure
	}
	return planStage, exitOK
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
