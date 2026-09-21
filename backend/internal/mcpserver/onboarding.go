package mcpserver

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
1. fishhawk_start_run — open a run for the issue. Pass runner_kind:local for the local dogfood loop (it defaults to github_actions). Pass working_dir as the ABSOLUTE path to the checkout this run executes in: YOU, the calling agent, resolve your own checkout (you are running inside one) rather than asking the operator for a path — and the later runner-spawning verbs INHERIT it, so you pass it once.
2. fishhawk_run_stage (plan) — the agent writes a plan. Blocks until the plan stage settles.
3. fishhawk_approve_plan — read the plan AND its advisory reviews first; approve, or fishhawk_reject_plan with a reason to replan.
4. fishhawk_dispatch_stage (implement) — execute the approved plan. On the local runner this is what spawns the runner; it does not auto-start. It returns a next_step pointing at fishhawk_await_stage (run_id + stage_id pre-filled) — call that to block until the stage settles instead of hand-polling. A client that BACKGROUNDS long tool calls needs no polling at all; dispatch_stage's real advantage is its in-band mid-stage scope-amendment channel, not the non-blocking part — and that channel is now OBSERVABLE from the recommended wait: fishhawk_await_stage also releases with status amendment_pending carrying the amendment row and a pre-filled fishhawk_decide_scope_amendment next_step (#2588), so no separate fishhawk_list_scope_amendments poll cycle is needed.
5. fishhawk_await_review — wait for the implement review to reach a terminal verdict.
6. When the workflow declares an acceptance stage: fishhawk_dispatch_stage (acceptance) after the review settles, await the verdict (fishhawk_get_run_status acceptance_stage_wait_status, or fishhawk_await_audit on acceptance_outcome_recorded), and merge only on a merge-eligible acceptance state: acceptance_passed (a validated pass), acceptance_not_validated (the stage short-circuited having verified ZERO criteria — mergeable, but acknowledge that in your merge verdict, #2347), acceptance_undecidable (the stage RAN but could not DECIDE one or more criteria with nothing failing — mergeable with NO arbitration, but say which criteria went undecided in your merge verdict, #2512), or acceptance_skipped_out_of_scope.
7. Approve the PR (gh pr review --approve, under your own GitHub identity), then fishhawk_merge_run — one verb that records your merge verdict, queues the squash merge, awaits the terminal run state, and surfaces your post-merge step.

Gate semantics (these decide when a verb is legal):
- Do not approve a plan while its review is still pending — wait for plan_review to clear.
- Wait for ALL configured reviewers. A feature_change run is reviewed by two agents concurrently; expect two verdicts and treat advisory disagreement as normal — you arbitrate.
- A mid-implement scope amendment is operator-gated: the agent requests paths, you decide. Name added files as dir/file.ext.
- A failed acceptance verdict leaves the stage 'succeeded' and routes through deterministic server-side triage (auto fix-up / re-run, bounded); paged dispositions are yours to arbitrate. Read the verdict from the acceptance_outcome_recorded audit entry, not the stage state.
- next_actions on the run status is the authoritative "what to do next" — prefer it over guessing.

Refinement intake (separate from the run loop): when you have a natural-language brief to decompose into an epic + children, drive fishhawk_draft_epic — one tool with five arms (open, preview, edit, approve/reject, file); approve and file are ARMS on it, not fishhawk_approve_plan. Its session_guidance names the next arm at each step. See the runbook's "Refinement intake loop" section.

Backlog grooming (also separate from the run loop): backlog_grooming is a NON-DIFF workflow — it proposes tracker mutations and parks at an approval gate. Approving that gate EXECUTES the approved mutations server-side; there is no separate apply step, so read the grooming report BEFORE you approve. Start it with trigger_source:on_demand (omit it and applies_to refuses the workflow and the run never starts) and drive its propose stage with the stage TYPE plan, not the stage id. See the runbook's "Backlog grooming loop" section.

Read the fishhawk://runbook resource for the full procedure and the edge-case playbook (local-drive dispatch, fixup re-dispatch, failed-run revive, decomposed-parent fan-out (run_children/consolidate_slices), drive_run loop shape, batch-as-campaign local drive (start_campaign/start_campaign_item_run/get_campaign_status), scope amendments, heterogeneous-review waits, post-failure clean-tree, refinement intake loop, backlog grooming loop).

Onboarding a NEW repository (no .fishhawk/workflows.yaml yet): read the fishhawk://onboarding-skill resource — a Claude Code SKILL.md-shaped walk through fishhawk_doctor → fishhawk_init → commit the spec.`

// runbookMarkdown is the long-form operator runbook, embedded as a
// product file so the binary serves it without a filesystem dependency.
// A renamed or missing runbook.md is a build-time failure (the
// //go:embed directive), and an empty file is caught by the unit
// assertion in onboarding_test.go.
//
//go:embed runbook.md
var runbookMarkdown string

// onboardingSkillMarkdown is the SKILL.md-shaped onboarding walk, embedded
// as a product file for the same reason runbookMarkdown is: the binary
// serves it without a filesystem dependency. A renamed or missing
// onboarding_skill.md is a build-time failure (the //go:embed directive),
// and an empty file is caught by the unit assertion in onboarding_test.go.
//
//go:embed onboarding_skill.md
var onboardingSkillMarkdown string

// runbookURI is the MCP resource URI the runbook is served under. The
// fishhawk:// scheme is non-empty and absolute, so srv.AddResource
// accepts it (it panics only on an invalid or empty-scheme URI).
const runbookURI = "fishhawk://runbook"

// onboardingSkillURI is the MCP resource URI the onboarding skill is
// served under — a distinct fishhawk:// URI from runbookURI.
const onboardingSkillURI = "fishhawk://onboarding-skill"

// registerOnboardingResources registers the readable fishhawk://runbook and
// fishhawk://onboarding-skill resources on srv. It is called on the single
// shared construction path (newServer) so both resources are
// transport-neutral — they cross the registration->transport seam
// identically on the stdio and streamable-HTTP transports.
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
	srv.AddResource(
		&mcp.Resource{
			URI:         onboardingSkillURI,
			Name:        "fishhawk-onboarding-skill",
			Title:       "Fishhawk repository onboarding skill",
			Description: "A Claude Code SKILL.md-shaped walk through fishhawk_doctor → fishhawk_init → committing the spec, for onboarding a repository that has no .fishhawk/workflows.yaml yet.",
			MIMEType:    "text/markdown",
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      onboardingSkillURI,
					MIMEType: "text/markdown",
					Text:     onboardingSkillMarkdown,
				}},
			}, nil
		},
	)
}
