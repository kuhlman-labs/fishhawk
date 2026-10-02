---
id: ADR-023
title: "Agent self-retry on transient stage failure"
status: accepted
date: 2026-05-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/401
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-023: Agent self-retry on transient stage failure

## Context

ADR-021 established that the v0 MCP tools are read-only because "the agent in the runner shouldn't approve its own work." ADR-022's addendum generalized this: **runner identity is read-only across all backends** (github_actions, local, k8s). Two distinct processes carry two distinct identities even on the same physical machine.

That posture is the right default. But it leaves one specific case awkward: **the agent can't retry its own failed stage** even when the failure is a known transient (flaky test, infrastructure hiccup, network blip). Today retry is operator-driven via `fishhawk run retry <stage-id>` or the new `fishhawk_retry_stage` MCP tool (#392 / E22.3), which only operator-side `fhk_*` apitokens can call.

This forces an awkward dance: the agent detects a transient failure, surfaces a tool-error message via its MCP read tools ("stage X failed with category A, please retry"), the operator (often the same human, sometimes seconds away from the agent) types one command. For interactive dev loops this is fine; for batch / unattended workflows it's a real friction point.

## Options

1. **Status quo: agent never retries.** Operator handles every retry. Simple, secure, but adds latency to batch workflows.
2. **Full write scope for the agent.** Reverts ADR-021's principle. Out of scope; rejected per ADR-022's addendum.
3. **Narrowly-scoped write capability: `write:retries`.** New mcptoken scope that authorizes only the retry endpoint. Agent can self-retry but can't approve / cancel / start. Approvals and other gates stay operator-only.

## Recommendation (initial — subject to ADR review)

Option 3, with explicit guards:

- New scope `write:retries` issued on mcptokens. Default issuance keeps the v0 posture (`mcp:read` only); the scope is opt-in via a workflow-spec field (e.g. `agent_self_retry: true` on the implement stage's executor block) or a server-side flag.
- The `/v0/stages/{id}/retry` handler's role check accepts mcptokens carrying this scope, but ONLY for retries against the issuing run's own stages (the mcptoken's subject `mcp:run:<id>` must match the stage's run_id). This prevents an mcptoken leaked to one run from retrying another run's stages.
- The existing `runs.max_retries_snapshot` cap (#280 / E16) bounds runaway retry loops on real (non-transient) failures.
- Retry audit rows attribute correctly: `actor_subject = mcp:run:<id>`, distinguishable from operator-driven retries (`github:<login>`).

## Why retries specifically (and not other writes)

The agent-self-retry case is different from agent-self-approval in one important way: **retry doesn't change a gating decision.** Approval = "a human looked at this and said yes." Retry = "the agent attempted this and the attempt failed transiently." The plan-approval gate fires *after* the eventual successful attempt; whether the agent retried twice or thirty times doesn't affect whether a human approved. The audit chain records the retries; the gate sees the final outcome.

This argues the principle isn't "agent has no writes" but "agent doesn't make gating decisions." Retries fit cleanly on the agent's side; approvals don't.

## Open design questions

- **Workflow-spec opt-in or default-on?** The retry-budget cap already bounds blast radius. Making it default-on saves friction; making it opt-in keeps the v0 surface narrow. Either is defensible.
- **Category restrictions?** Should the scope only authorize category-A retries (agent failure)? Category C (infrastructure) seems fine too. Category D (SLA timeout, gate-rejected) is approval-shaped; should be excluded.
- **Cancel parity?** Cancel is similar to retry in that it doesn't change a gating decision — it terminates the run. Worth considering whether `write:cancels` follows the same logic, or whether cancel is special enough to stay operator-only.
- **Backend's `RetryStage` already classifies retry-not-applicable per category** (per `run.RetryStage`). The mcptoken-scoped path could rely on that classification rather than introducing a separate filter.

## Decision

**Adopt Option 3 (narrowly-scoped `write:retries` capability)** with the following resolved parameters:

| Knob | Pick |
|---|---|
| Workflow-spec opt-in vs default-on | **Opt-in** via `executor.agent_self_retry: true` (additive boolean on the workflow spec; defaults `false` for backward compat). |
| Failure-category filter | **Categories A (agent) and C (infrastructure) only.** Category B (constraint/policy violation) excluded — re-shipping the same bytes won't help; the bad output stays bad. Category D (SLA timeout / gate-rejected) excluded — approval-shaped, not retry-shaped. |
| Subject binding | Handler check requires the mcptoken's `mcp:run:<id>` subject to **match the target stage's `run_id`**. An mcptoken leaked from one run cannot retry another run's stages. |
| Blast-radius cap | **Reuse the existing `runs.max_retries_snapshot` cap (#280 / E16).** No new bound; the budget exists. |
| Cancel parity | **Not extended to cancel** in this ADR. Cancel terminates work that could otherwise complete; "give up early" isn't an obviously safe agent reflex. Open as a separate decision if a use case surfaces. |
| Category-classification source | **Reuse `run.RetryStage`'s existing per-category retry-applicability classification** rather than introducing a parallel filter in the scope-check path. |
| Retry receipt (per Keesan12's comment) | Each retry audit entry carries a machine-readable payload with: `prior_failure_class`, `retry_ordinal`, `remaining_budget`, `admissibility_reason`. Compliance consumers can read the audit chain and understand WHY each retry was admissible without spelunking backend source. |

Resolved on 2026-05-26. Three implementation issues filed (see Children).

## Consequences

- The agent-driven batch workflow becomes viable without an operator in the loop for transient (category A/C) failures.
- The audit log carries `actor_subject = mcp:run:<id>` for agent-driven retries vs `github:<login>` for operator-driven — compliance consumers can filter.
- The audit retry receipt makes the "why this retry was allowed" question first-class observable rather than implicit in backend logic.
- The scope model gains one new flavor (`write:retries`). Worth a careful look to make sure it doesn't compose with other write scopes in surprising ways.
- Workflow-v0 schema gains `executor.agent_self_retry` (optional boolean, additive — no major version bump per the schema-evolution discipline in #472).
- mcptoken issuance grows a per-run conditional scope grant based on the spec field; existing mcptokens have a TTL, so the next run picks up the new behavior naturally.

## Out of scope

- Self-cancel, self-start, self-approve. Approvals are explicitly excluded per ADR-021. The others can be considered separately if a real use case surfaces.
- Backwards compat for existing mcptokens — they have a TTL, so the next run gets the new scopes naturally.

## Children (implementation follow-ups)

- #533 — Workflow-v0 schema: `executor.agent_self_retry` (impl 1/3, prerequisite for the other two).
- #534 — `write:retries` scope plumbing for `/v0/stages/{id}/retry` (impl 2/3, depends on #533).
- #535 — Runner: detect category-A/C failures and self-retry (impl 3/3, depends on #533 and #534).

## Related

- ADR-021 / #322 (MCP server v0 read-only) — the principle this ADR carves an exception to.
- ADR-022 / #388 (Pluggable runner backends + runner_kind) — the addendum that motivated this follow-up.
- #392 / E22.3 (`fishhawk_retry_stage` MCP tool) — the operator-side surface; this ADR opens the agent-side parallel.
- #280 / E16 (max_retries_snapshot) — the budget cap that bounds blast radius.
- #472 / ADR-026 — schema-evolution discipline; the `executor.agent_self_retry` field follows that pattern.
