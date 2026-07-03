package main

import (
	"context"
	_ "embed"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// onboardingInstructions is the server `instructions` field returned on
// every MCP `initialize`. It is the in-band onboarding for a connecting
// client whose agent holds no operator memory: a concise happy-path verb
// sequence plus the gate semantics that decide when each verb is legal.
//
// Kept deliberately short (BRAND_FOUNDATIONS §5: direct, technical, no
// fluff). The long-form procedure and the edge-case playbook live in the
// fishhawk://runbook resource — pointed at from the last line — so this
// string stays a glance-able map, not a manual.
const onboardingInstructions = `Fishhawk drives a software change through gated stages. You are the operator: the agent proposes, you decide at each gate.

Happy-path loop (one issue, one run):
1. fishhawk_start_run — open a run for the issue. Pass runner_kind:local for the local dogfood loop (it defaults to github_actions).
2. fishhawk_run_stage (plan) — the agent writes a plan. Blocks until the plan stage settles.
3. fishhawk_approve_plan — read the plan AND its advisory reviews first; approve, or fishhawk_reject_plan with a reason to replan.
4. fishhawk_dispatch_stage (implement) — execute the approved plan. On the local runner this is what spawns the runner; it does not auto-start.
5. fishhawk_await_review — wait for the implement review to reach a terminal verdict.
6. When the workflow declares an acceptance stage: fishhawk_dispatch_stage (acceptance) after the review settles, await the verdict (fishhawk_get_run_status acceptance_stage_wait_status, or fishhawk_await_audit on acceptance_outcome_recorded), and merge only on the acceptance_passed state.
7. Approve the PR, then merge it, then run your post-merge step.

Gate semantics (these decide when a verb is legal):
- Do not approve a plan while its review is still pending — wait for plan_review to clear.
- Wait for ALL configured reviewers. A feature_change run is reviewed by two agents concurrently; expect two verdicts and treat advisory disagreement as normal — you arbitrate.
- A mid-implement scope amendment is operator-gated: the agent requests paths, you decide. Name added files as dir/file.ext.
- A failed acceptance verdict leaves the stage 'succeeded' and routes through deterministic server-side triage (auto fix-up / re-run, bounded); paged dispositions are yours to arbitrate. Read the verdict from the acceptance_outcome_recorded audit entry, not the stage state.
- next_actions on the run status is the authoritative "what to do next" — prefer it over guessing.

Refinement intake (separate from the run loop): when you have a natural-language brief to decompose into an epic + children, drive fishhawk_draft_epic — one tool with five arms (open, preview, edit, approve/reject, file); approve and file are ARMS on it, not fishhawk_approve_plan. Its session_guidance names the next arm at each step. See the runbook's "Refinement intake loop" section.

Read the fishhawk://runbook resource for the full procedure and the edge-case playbook (local-drive dispatch, fixup re-dispatch, scope amendments, heterogeneous-review waits, post-failure clean-tree, refinement intake loop).`

// runbookMarkdown is the long-form operator runbook, embedded as a
// product file so the binary serves it without a filesystem dependency.
// A renamed or missing runbook.md is a build-time failure (the
// //go:embed directive), and an empty file is caught by the unit
// assertion in onboarding_test.go.
//
//go:embed runbook.md
var runbookMarkdown string

// runbookURI is the MCP resource URI the runbook is served under. The
// fishhawk:// scheme is non-empty and absolute, so srv.AddResource
// accepts it (it panics only on an invalid or empty-scheme URI).
const runbookURI = "fishhawk://runbook"

// registerOnboardingResources registers the readable fishhawk://runbook
// resource on srv. It is called on the single shared construction path
// (newServer) so the resource is transport-neutral — it crosses the
// registration->transport seam identically on the stdio and
// streamable-HTTP transports.
func registerOnboardingResources(srv *mcp.Server) {
	srv.AddResource(
		&mcp.Resource{
			URI:         runbookURI,
			Name:        "fishhawk-runbook",
			Title:       "Fishhawk operator runbook",
			Description: "The full loop-driving procedure plus the edge-case playbook (local-drive dispatch, fixup re-dispatch, scope amendments, heterogeneous-review waits, post-failure clean-tree).",
			MIMEType:    "text/markdown",
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      runbookURI,
					MIMEType: "text/markdown",
					Text:     runbookMarkdown,
				}},
			}, nil
		},
	)
}
