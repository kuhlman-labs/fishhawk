package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// reviewGateState is the state a human-executor review gate parks at
// (backend/internal/run.StageStateAwaitingApproval). The CLI is a separate Go
// module and cannot import backend/internal/run, so this local constant is the
// drift-prevention mechanism — the same duplication deploy.go's deployGateState
// carries, and TestApproveReviewGate_NoAwaitingReviewStage / the happy path
// both redden if the string is wrong.
const reviewGateState = "awaiting_approval"

// reviewGateSettledState is the state an ALREADY-APPROVED review gate carries.
// It is what separates the verb's duplicate no-op branch from its "no review
// gate" error: a successful approval advances the review stage out of
// reviewGateState, so a repeated invocation can never reach the approvals
// endpoint and must be rendered as the labeled no-op it is rather than as a
// missing gate.
const reviewGateSettledState = "succeeded"

// runApproveReviewGate implements
// `fishhawk approve-review-gate <run-id> --attest "..." [--output text|json]`.
//
// It replaces the raw curl documented in docs/GROOMING_RUNBOOK.md §5 for
// approving a human-executor review gate (backlog_grooming's `confirm` stage,
// E54.55 / #3051). The ergonomic win over the curl is stage resolution: the
// operator passes the RUN id and the verb finds the run's `type: review` stage
// parked at awaiting_approval, rather than having to fetch the stage id first.
//
// There is deliberately NO MCP verb for this gate. An MCP tool runs under the
// delegated operator-agent identity the gate refuses (403
// operator_agent_forbidden), which is the gate working as declared — see
// backend/internal/server/review_gate_admission.go refuseAgentOnHumanReviewGate.
func runApproveReviewGate(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk approve-review-gate"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	attest := fs.String("attest", "", "REQUIRED attestation recorded on the approval: state what you checked on the FORGE, not what the summary said")
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

	// ORDERING IS LOAD-BEARING: this guard runs BEFORE newClient(cf) and
	// before any client call, so a refused invocation dials nothing at all.
	// The backend would refuse the same input 400 attestation_required, but
	// only after a round trip; refusing locally keeps a typo out of the wire
	// entirely. Do NOT hoist the client construction above this check —
	// TestApproveReviewGate_EmptyAttest_NeverDialsBackend asserts the fake
	// received ZERO requests and reddens if it moves.
	attestation := strings.TrimSpace(*attest)
	if attestation == "" {
		_, _ = fmt.Fprintf(stderr,
			"%s: --attest is required — the attestation IS what this gate records: state what you checked on the FORGE (the issues/labels/links you actually looked at), not what the run summary claimed\n", name)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	client := newClient(cf)

	reviewStage, settled, exitCode := resolveReviewGateStage(ctx, client, name, runID, stderr)
	if reviewStage == nil {
		return exitCode
	}
	if settled {
		// Duplicate no-op, resolved CLIENT-side. A successful approval
		// advances the review stage out of awaiting_approval, so a repeated
		// invocation cannot reach the approvals endpoint at all — rendering
		// this as "no review gate" would read as a missing gate rather than
		// as the already-done work it is. Nothing is dialed beyond the stage
		// list that established it.
		_, _ = fmt.Fprintf(stderr,
			"%s: no-op — the review gate on run %s is already approved (stage %s is %s); the prior approval stands and no new decision was submitted\n",
			name, runID, reviewStage.ID, reviewStage.State)
		if *outputFmt == "json" {
			if err := json.NewEncoder(stdout).Encode(httpclient.ApprovalResult{
				Stage:               *reviewStage,
				DuplicateSubmission: true,
			}); err != nil {
				_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
				return exitFailure
			}
			return exitOK
		}
		printStage(stdout, reviewStage)
		return exitOK
	}

	res, err := client.SubmitApproval(ctx, reviewStage.ID, httpclient.SubmitApprovalInput{
		Decision: httpclient.ApprovalApprove,
		Comment:  attestation,
	})
	if err != nil {
		// Always print the backend envelope VERBATIM first: the server's
		// message is the authoritative statement of what it refused, and the
		// explanations below supplement it rather than replace it.
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		if explain := explainReviewGateRefusal(err); explain != "" {
			_, _ = fmt.Fprintf(stderr, "%s: %s\n", name, explain)
		}
		var apiErr *httpclient.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "review_stage_managed_by_github" {
			if explain := explainReviewGateAdmission(apiErr.Details); explain != "" {
				_, _ = fmt.Fprintf(stderr, "%s: %s\n", name, explain)
			}
		}
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
			// #986: the backend's own same-stage-same-subject labeling. This
			// is the narrower window the client-side settled branch above
			// cannot see (a concurrent approval landing between the stage
			// list and this POST). Never render a no-op as a normal result.
			_, _ = fmt.Fprintf(stderr,
				"%s: duplicate submission — prior %s decision (%s) stands; stage state unchanged\n",
				name, res.PriorDecision, res.PriorSubmittedAt)
		}
		printStage(stdout, &res.Stage)
	}
	return exitOK
}

// resolveReviewGateStage resolves which review stage (if any) this verb should
// act on, in the fixed order the operator's approval condition fixes:
//
//  1. a review stage at awaiting_approval  -> (stage, false, exitOK): submit.
//  2. else a review stage already succeeded -> (stage, true, exitOK): the
//     caller renders the labeled no-op and dials the approvals endpoint not at
//     all.
//  3. else                                  -> (nil, false, exitFailure): the
//     actionable "no review gate" error naming the status verb.
//
// A failed stage list returns (nil, false, exitOnAPIError).
func resolveReviewGateStage(ctx context.Context, client *httpclient.Client, name string, runID uuid.UUID, stderr io.Writer) (*httpclient.Stage, bool, int) {
	stages, err := client.ListRunStages(ctx, runID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: list stages: %v\n", name, err)
		return nil, false, exitOnAPIError(err)
	}
	if stage := findReviewStageInState(stages.Items, reviewGateState); stage != nil {
		return stage, false, exitOK
	}
	if stage := findReviewStageInState(stages.Items, reviewGateSettledState); stage != nil {
		return stage, true, exitOK
	}
	_, _ = fmt.Fprintf(stderr,
		"%s: run %s has no review stage awaiting approval (check `fishhawk_get_run_status`, or `fishhawk run status %s`)\n",
		name, runID, runID)
	return nil, false, exitFailure
}

// findReviewStageInState returns the first review stage in the given state, or
// nil. The stage list is sequence-ascending and a v0 workflow declares at most
// one review stage, so the first match is authoritative.
func findReviewStageInState(stages []httpclient.Stage, state string) *httpclient.Stage {
	for i := range stages {
		if stages[i].Type == "review" && stages[i].State == state {
			return &stages[i]
		}
	}
	return nil
}

// explainReviewGateRefusal renders an operator-readable meaning for each of the
// three refusals this gate can produce, so the raw envelope code is not the
// only thing an operator has to go on. Returns "" for any other code, leaving
// the verbatim envelope as the whole message.
//
// The two agent-identity refusals state the gate is human-only BY DESIGN. That
// distinction matters: an operator who reads `self_decision` as a
// misconfiguration will go looking for a broken token, when what the backend is
// reporting is the workflow spec's own `not: [agent]` constraint being honored.
func explainReviewGateRefusal(err error) string {
	var apiErr *httpclient.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch apiErr.Code {
	case "attestation_required":
		return "the approve carried an empty or whitespace-only comment — the attestation IS what this gate records; pass --attest with what you checked on the forge"
	case "self_decision":
		return "a run-bound agent token may not approve a human-executor review gate, not even for its own run: this gate declares executor: human with not: [agent], so it is human-only BY DESIGN and this is NOT a misconfiguration — re-run with a HUMAN-held operator credential"
	case "operator_agent_forbidden":
		return "a delegated operator-agent token is refused BY DESIGN: this gate declares executor: human with not: [agent], and an MCP tool runs under exactly this delegated identity — which is why no MCP verb exists for this gate — so it needs a HUMAN-held operator credential, not a misconfiguration to fix"
	}
	return ""
}

// explainReviewGateAdmission renders the 409 review_stage_managed_by_github
// admission ladder: details.admission_reason names WHICH leg of the admission
// conjunction failed (backend/internal/server/review_gate_admission.go
// reviewGateAdmitReason.String). A missing or non-string admission_reason
// renders nothing — the verbatim envelope still printed, and inventing a reason
// would be worse than saying none.
func explainReviewGateAdmission(details map[string]any) string {
	reason, _ := details["admission_reason"].(string)
	if reason == "" {
		return ""
	}
	if reason == "pull_request_managed" {
		return "admission_reason=pull_request_managed — this is a PR-merge-managed review stage, not a human-executor gate; approve it by merging the pull request"
	}
	return fmt.Sprintf(
		"admission_reason=%s — the gate was not admitted: a fail-closed resolution failure against the run's CACHED workflow spec, not a credential problem",
		reason)
}
