package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runDeploy dispatches `fishhawk deploy <subcommand>`. It mirrors
// runPlan's shape and drives the E23.7 deploy API surface from the
// terminal (E23.8 / #1388): `status` shows the deploy stage state plus
// the persisted deployment artifact, `approve`/`reject` decide the
// deploy stage's pre-execution gate (the same approvals endpoint as
// plan; write:deploy is enforced server-side), and `rollback` invokes
// the rollback sub-action endpoint.
func runDeploy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk deploy: subcommand required (status|approve|reject|rollback)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		return deployStatus(rest, stdout, stderr)
	case "approve":
		return deployApprove(rest, stdout, stderr)
	case "reject":
		return deployReject(rest, stdout, stderr)
	case "rollback":
		return deployRollback(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk deploy: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// Deployment is the CLI-side projection of the deployment artifact's
// content, decoded from Artifact.Content. It mirrors the backend's
// deploymentBody (backend/internal/server/deployment.go): the artifact
// is stored verbatim with no schema_version, so the field set is kept
// CLI-side. A future additive field decodes harmlessly (unknown JSON
// keys are ignored).
type Deployment struct {
	Environment    string `json:"environment"`
	Ref            string `json:"ref"`
	ExternalRunURL string `json:"external_run_url"`
	Outcome        string `json:"outcome"`
	RollbackHandle string `json:"rollback_handle,omitempty"`
	RollbackAction string `json:"rollback_action,omitempty"`
}

// deployStatusOutput is the `--output json` shape for `deploy status`:
// the deploy stage plus the decoded deployment artifact (nil when not
// yet recorded).
type deployStatusOutput struct {
	Stage      httpclient.Stage `json:"stage"`
	Deployment *Deployment      `json:"deployment"`
}

// deployStatus implements `fishhawk deploy status <run-id> [--output text|json]`.
// It resolves the run's deploy stage (any state), reads the deployment
// artifact attached to that stage if one exists, and renders both.
func deployStatus(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk deploy status"
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
		_, _ = fmt.Fprintf(stderr, "%s: <run-id> required\n", name)
		return exitUsage
	}
	runID, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %q is not a UUID: %v\n", name, positionals[0], err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	client := newClient(cf)

	stages, err := client.ListRunStages(ctx, runID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: list stages: %v\n", name, err)
		return exitOnAPIError(err)
	}
	deployStage := findDeployStage(stages.Items)
	if deployStage == nil {
		_, _ = fmt.Fprintf(stderr, "%s: run %s has no deploy stage\n", name, runID)
		return exitFailure
	}

	dep, err := fetchDeployment(ctx, client, deployStage.ID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: list artifacts: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(deployStatusOutput{Stage: *deployStage, Deployment: dep}); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		printStage(stdout, deployStage)
		if dep == nil {
			_, _ = fmt.Fprintln(stdout, "deployment:     (not yet recorded)")
		} else {
			printDeployment(stdout, dep)
		}
	}
	return exitOK
}

// deployApprove implements `fishhawk deploy approve <run-id> [--reason ...] [--output text|json]`.
// Resolves the deploy stage awaiting approval and POSTs an approve
// decision through the shared approvals endpoint. The write:deploy
// scope is enforced server-side (ADR-038 / #1390); the CLI sends the
// token unchanged and surfaces any 403 envelope verbatim.
func deployApprove(args []string, stdout, stderr io.Writer) int {
	return deployDecision("fishhawk deploy approve", httpclient.ApprovalApprove,
		args, stdout, stderr)
}

// deployReject implements `fishhawk deploy reject <run-id> [--reason ...] [--output text|json]`.
// Same flow as deployApprove but submits a reject decision, with the
// same missing-reason soft warning planReject uses.
func deployReject(args []string, stdout, stderr io.Writer) int {
	return deployDecision("fishhawk deploy reject", httpclient.ApprovalReject,
		args, stdout, stderr)
}

// deployDecision is the shared body of `deploy approve` / `deploy reject`,
// modeled on planDecision. It owns flag parsing, run-id validation,
// deploy-stage resolution, the approvals POST, and output formatting.
// The two verbs differ only in the decision and the soft-warning
// reject opts into for a missing --reason.
func deployDecision(name string, decision httpclient.ApprovalDecision, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	reason := fs.String("reason", "", "optional comment recorded on the approval row")
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
		_, _ = fmt.Fprintf(stderr, "%s: <run-id> required\n", name)
		return exitUsage
	}
	runID, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %q is not a UUID: %v\n", name, positionals[0], err)
		return exitUsage
	}
	if decision == httpclient.ApprovalReject && *reason == "" {
		// Soft warning, mirroring planReject: a reject without a reason
		// is wire-legal but records an empty audit comment. Don't fail —
		// just make the loss visible.
		_, _ = fmt.Fprintf(stderr,
			"%s: warning: --reason not provided; the approval row will record an empty comment\n", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	client := newClient(cf)

	deployStage, exitCode := resolveDeployStage(ctx, client, name, runID, stderr)
	if deployStage == nil {
		return exitCode
	}

	res, err := client.SubmitApproval(ctx, deployStage.ID, httpclient.SubmitApprovalInput{
		Decision: decision,
		Comment:  *reason,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		if res.DuplicateSubmission {
			// #986: never render a no-op as a normal result. The prior
			// decision stands and the stage didn't move.
			_, _ = fmt.Fprintf(stderr,
				"%s: duplicate submission — prior %s decision (%s) stands; stage state unchanged\n",
				name, res.PriorDecision, res.PriorSubmittedAt)
		}
		printStage(stdout, &res.Stage)
	}
	return exitOK
}

// deployRollback implements `fishhawk deploy rollback <run-id> [--output text|json]`.
// It invokes the rollback sub-action endpoint (POST
// /v0/runs/{id}/deployment/rollback), which re-dispatches the delegating
// pipeline down its rollback path and returns the rollback run handle.
// Server-side preconditions (deploy_not_settled 409, rollback_unconfigured
// 422, insufficient_scope 403) surface verbatim via exitOnAPIError.
func deployRollback(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk deploy rollback"
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
		_, _ = fmt.Fprintf(stderr, "%s: <run-id> required\n", name)
		return exitUsage
	}
	runID, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %q is not a UUID: %v\n", name, positionals[0], err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	res, err := newClient(cf).RollbackDeployment(ctx, runID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		_, _ = fmt.Fprintf(stdout, "target:           %s\n", res.Target)
		_, _ = fmt.Fprintf(stdout, "stage_id:         %s\n", res.StageID)
		if res.GHARunID != 0 {
			_, _ = fmt.Fprintf(stdout, "gha_run_id:       %d\n", res.GHARunID)
		}
		if res.ExternalRunURL != "" {
			_, _ = fmt.Fprintf(stdout, "external_run_url: %s\n", res.ExternalRunURL)
		}
		_, _ = fmt.Fprintf(stdout, "message:          %s\n", res.Message)
	}
	return exitOK
}

// resolveDeployStage finds the deploy stage awaiting approval on the
// given run. Returns (stage, exitOK) on success; (nil, exitCode) when no
// awaiting-approval deploy stage exists or the list call failed. Mirrors
// resolvePlanStage's shape and error wording.
func resolveDeployStage(ctx context.Context, client *httpclient.Client, name string, runID uuid.UUID, stderr io.Writer) (*httpclient.Stage, int) {
	stages, err := client.ListRunStages(ctx, runID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: list stages: %v\n", name, err)
		return nil, exitOnAPIError(err)
	}
	deployStage := findAwaitingApprovalDeployStage(stages.Items)
	if deployStage == nil {
		_, _ = fmt.Fprintf(stderr,
			"%s: run %s has no deploy stage awaiting approval (check `fishhawk deploy status %s`)\n",
			name, runID, runID)
		return nil, exitFailure
	}
	return deployStage, exitOK
}

// findDeployStage returns the first deploy stage in the list, in any
// state. The deploy stage type is unique within a workflow in v0, so the
// first match is authoritative. Returns nil when the run has none.
func findDeployStage(stages []httpclient.Stage) *httpclient.Stage {
	for i := range stages {
		if stages[i].Type == "deploy" {
			return &stages[i]
		}
	}
	return nil
}

// deployGateState is the deploy pre-execution gate state the backend parks at
// (backend/internal/run/run.go StageStateAwaitingDeployApproval). The CLI is a
// separate Go module and cannot import the backend run package, so this local
// constant is the drift-prevention mechanism.
const deployGateState = "awaiting_deploy_approval"

// findAwaitingApprovalDeployStage returns the first deploy stage parked at
// awaiting_deploy_approval — the pre-execution gate `deploy approve`/`reject`
// decide. Returns nil when no such stage exists.
func findAwaitingApprovalDeployStage(stages []httpclient.Stage) *httpclient.Stage {
	for i := range stages {
		if stages[i].Type == "deploy" && stages[i].State == deployGateState {
			return &stages[i]
		}
	}
	return nil
}

// fetchDeployment lists the deploy stage's artifacts and decodes the
// deployment artifact's content, or returns (nil, nil) when no deployment
// artifact is attached yet.
func fetchDeployment(ctx context.Context, client *httpclient.Client, stageID uuid.UUID) (*Deployment, error) {
	artifacts, err := client.ListStageArtifacts(ctx, stageID)
	if err != nil {
		return nil, err
	}
	for _, a := range artifacts {
		if a.Kind != "deployment" {
			continue
		}
		var dep Deployment
		if err := json.Unmarshal(a.Content, &dep); err != nil {
			return nil, fmt.Errorf("decode deployment artifact: %w", err)
		}
		return &dep, nil
	}
	return nil, nil
}

// printDeployment renders a Deployment as a small block of human-readable
// lines, following printStage's field-alignment shape.
func printDeployment(w io.Writer, d *Deployment) {
	_, _ = fmt.Fprintf(w, "environment:    %s\n", d.Environment)
	_, _ = fmt.Fprintf(w, "ref:            %s\n", d.Ref)
	_, _ = fmt.Fprintf(w, "external_url:   %s\n", d.ExternalRunURL)
	_, _ = fmt.Fprintf(w, "outcome:        %s\n", d.Outcome)
	if d.RollbackHandle != "" {
		_, _ = fmt.Fprintf(w, "rollback_handle: %s\n", d.RollbackHandle)
	}
	if d.RollbackAction != "" {
		_, _ = fmt.Fprintf(w, "rollback_action: %s\n", d.RollbackAction)
	}
}
