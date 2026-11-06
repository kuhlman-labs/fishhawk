# Codex-only workflow contingency

Issue #5094 prepares the repository workflow for Anthropic quota exhaustion.
Activation requires the routing fix from #5095 and acceptance model transport
from #5096 to be deployed. #5095 merged in PR #5098; its binaries still need a
guarded rollout. Acceptance currently lacks model transport, so provider
selection alone does not make that pin effective. A check-gated review stage
spawns no provider invocation; its declared model is not an active invocation
pin. An unmerged preparation branch changes no live run.

The Captain authorized activation after the hotfix merge and campaign
resumption if Anthropic quota is exhausted. Keep this branch unmerged until
that condition, the rollout prerequisites and the human approval gate are met.

## Workload mapping

| Previous assignment | Codex assignment | Reasoning effort |
|---|---|---|
| Opus planning and feature/hotfix implementation | `gpt-6-astra` | Existing `xhigh` |
| Opus acceptance and routine implementation | `gpt-6-astra` | Existing `medium` |
| Fable standard reviewer | `gpt-6.1-sol` | Existing `high` |
| Sonnet architect and security personas | `gpt-6.1-sol` | Existing `high` |
| Implicit grooming executor | `gpt-6.1-sol` | Existing `high` |
| Routine review declaration (no provider invocation) | `gpt-6.1-sol` | Unused for this gate |
| Implicit upkeep executor | `gpt-6.1-sol` | Explicit `high` |
| Existing Astra standard reviewer | `gpt-6-astra` | Existing `high` |

Every agent executor uses `agent: codex`; every standard reviewer and persona
uses `provider: codex`. Hotfix pins the same agent and model as the feature
workflow explicitly and inherits the remaining stage settings. These explicit
pins also work with backend model readers predating #5095's inheritance fix;
update both workflows when changing their model assignments.
Human gates and delegated deploy execution retain their existing executors.
The routine review declaration describes a check-gated auto-merge stage. The
prompt builder has no `review` template, and that gate takes the orchestrator's
auto-merge path before agent dispatch. Its configured agent/model must not be
counted as an observed provider spawn. The existing #1416 review-model ladder
and persisted resolutions for implement reviewers remain unchanged.

This is a mapping by workload role, not a claim of measured behavioral or
runtime equivalence. [OpenAI's model guidance](https://learn.chatgpt.com/docs/models)
positions Astra for demanding workflows and Sol for repeated, long-running work.
[The model guide](https://developers.openai.com/api/docs/guides/latest-model)
documents the selected reasoning levels. Both distinct standard reviewer models
remain; their shared provider can produce correlated failures or quota limits.

Historical Sonnet catch-rate results in the workflow comments and
[evidence record](compliance/planreview-catchrate-evidence.md) remain historical.
No Sol catch-rate remeasurement is supplied by this configuration change. The
persona remits, conventions and medium severity caps remain intact. All gates,
reviewer counts, timeouts, scope constraints, concurrency limits, retry limits,
verification commands, delegation and budgets retain their existing values.

Reviewer-configuration evaluation remains pending under ADR-118 in
[#4667](https://github.com/kuhlman-labs/fishhawk/issues/4667) and
[#4697](https://github.com/kuhlman-labs/fishhawk/issues/4697). The current
`check-review-prompt-eval` reports no changed review-prompt path for this
two-file preparation; that result supplies no measurement of Sol or the new
reviewer configuration. The historical evidence is not evidence for these
replacement models.

## Rollout and existing runs

1. Validate the workflow with the product validator, including resolved hotfix
   inheritance. Check that every effective agent assignment uses Codex and that
   stripping only provider/model changes and the explicit upkeep effort leaves
   the previous workflow configuration unchanged. Run the repository verify gate
   when its shared line is free; do not start a competing verify.
2. Confirm the host dispatch path uses the routing fix from #5095. The explicit
   pins above avoid depending on its backend inheritance repair to select these
   repository models, but do not claim that repair is deployed until the backend
   version proves it. Follow the live-run guards for any backend rollout.
   Confirm #5096's model transport is active for acceptance;
   schema resolution alone is not proof that a declared model reaches a binary.
   Preserve the existing review-agent override behavior separately: #1416's
   review model channel controls reviewer invocations, not standalone execution.
   Confirm the deployment enables the Codex reviewer and allows the selected
   models. Verify both models through bounded authenticated CLI probes using the
   deployment's normal credential mechanism; never print or change credentials.
   A CLI version probe alone does not prove account access or quota.
3. Have the Captain approve the prepared PR through `human_led_change` before
   merging. Do not enable auto-merge as part of preparation.
4. Admit new runs against the merged spec and inspect their recorded provider,
   model, effort and actual reviewer dispatches. A missing reviewer or quota
   failure is not a successful review.

Existing runs cache their admission workflow. Merging this file does not change
Campaign 5's existing run specs, later reviews, retries or the children those
parents create. Model-only gate overrides do not change the executor's provider.
If an existing run encounters exhausted Anthropic quota, hold its next dispatch
and use a separately reviewed, supported per-run transition. This change does
not provide such a transition: do not edit stored specs directly, silently
replace campaign runs, reset budgets, cancel live work or restart the shared
stack to simulate one. Continue using Campaign 5's existing campaign ID.

The runner's Codex adapter starts fix-up invocations fresh instead of resuming a
Claude session. Its normal prompt and artifacts remain the context source.
Plan JSON still passes the runner's artifact validation; this configuration
change does not add Claude's CLI-side structured-output enforcement to Codex.
These adapter differences are not resolved or hidden by model selection.

This scope covers `.fishhawk/workflows.yaml`. Independent crew-role defaults,
live evaluation generators and other Anthropic clients are outside it; it does
not establish that the entire deployment has stopped using Anthropic.

## Rollback

Revert the configuration commit through a reviewed PR to restore the previous
assignments for future admissions. Existing Codex-pinned runs keep their own
cached specs. Rollback has the same per-run transition limit as rollout and
does not require cancelling Campaign 5 or restarting its live stack.
