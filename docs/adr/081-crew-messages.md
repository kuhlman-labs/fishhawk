---
id: ADR-081
title: "Crew messages: a closed set of typed, role-addressed messages on the audit chain, with consults answered mid-stage and disagreement escalated to the captain"
status: accepted
date: 2026-09-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/3727
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-081: Crew messages: a closed set of typed, role-addressed messages on the audit chain, with consults answered mid-stage and disagreement escalated to the captain

## Context

ADR-080 #3693 (accepted 2026-09-26) makes crew communication an alpha theme (charter §2, T10): *agents consult and flag one another through typed, addressed messages on the audit chain. A message is advice, never an order; a disagreement escalates to the captain rather than looping.* E77 #3698 carries the work. This ADR settles its shape before children are filed.

Today the crew talks hub-and-spoke through stages, and every channel that exists is narrow and typed:

| Channel | Where | Delivery | Binding? |
|---|---|---|---|
| `clarification_request` (plan sibling artifact; `recommended_default`, `tradeoffs`) | `docs/spec/clarification-request-v1.*`; answered via `POST /v0/stages/{id}/clarification` → `clarification_answered` | stage parks at `awaiting_input`; answers reloaded by `loadClarificationAnswers` | yes, once the captain answers |
| Reviewer concerns | `backend/internal/concern` state machine; `prompt.FixupConcern` | gate view; fix-up prompt | only when the operator routes them (`concern_ids`) |
| Scope amendments | `backend/internal/scopeamendment`; `/v0/runs/{id}/scope-amendments` + `/decision` | REST with the run-bound token; the only **mid-stage** agent→captain round trip | yes, once decided |
| Operator pages | ADR-040 operator agent; `issuecomment/ping.go` | issue thread | n/a |

There is no lateral channel. A planner cannot ask the architect whether a plan contradicts a settled decision, cannot ask the historian whether something like this was decided before, and security cannot flag a dependency on another run.

**Constraints found in the code (these shape the options):**

- **Stage agents cannot be relied on to have Fishhawk's MCP tools.** The runner passes no `--mcp-config` (ADR-021, #2703). An agent reliably has only `FISHHAWK_API_TOKEN` and the backend URL. A channel that stage agents must *use* has to be REST with the run-bound token.
- **Only text rendered by `prompt.Build` is covered by the injection-containment gate** (`backend/internal/agenteval`, 6 attack classes; `TestInjectionCorpus_ContainedInEveryReviewedRender`). Text that reaches an agent any other way, such as an HTTP response body, is outside the evidence in `docs/compliance/prompt-injection-evidence.md`.
- **The implement prompt never ingests untrusted text** (ARCHITECTURE §6 invariant #8): the approval gate is the trust boundary.
- **Escalation is thinner than E77 assumes.** `requirement_arbitration` has one detector (`server/autodrive.go` `requirementArbitrationOpen`, an open `requirement`-category concern) and is read only by the campaign auto-driver. `spec/v2autonomy.go` `expandTier(TierLow)` has an **empty** page list, so "`page_human_on` at every tier" holds only for medium and high.
- **Run-bound tokens** (`server/mcptoken.go` `handleIssueMCPToken`, subject `mcp:run:<uuid>`) get scopes by executing stage type. Decision endpoints require `write:stages` and reject run tokens (`self_decision`, `agent_token_required`).
- **Responders can run inside fishhawkd.** Plan reviews already do: `server/plan.go` `runPlanReviewLoop` invokes claudecode, codex or anthropic adapters, grounded read-only per ADR-078 #2519.
- **Reusable pieces:** the audit chain (`audit.AppendChained`, `AppendGlobalChained` for run-less anchors, `AppendChainedUnderBudget` for bounded counters, `AppendChainedAnchored`); the chain-authoritative-plus-derived-table pattern (`scopeamendment`, `concern`); the untrusted-text envelopes and `neutralizeEnvelopeDelimiters` in `backend/internal/prompt/prompt.go`; the render/attribution/cap half of E55's `backend/internal/repodoc`; `applyAndFileWorkItem` for filing.
- **Boundaries:** N6 (closed stage set: responders are reviewer-style invocations, not stages), N7 (no cross-repository messaging), N1 (#654's planner→coder→tester mesh is parked; this must stay governance, not a coding mesh).

## Options

**D1: when a consult is answered**

1. **Synchronous, mid-stage.** The sender POSTs a consult with its run token. fishhawkd runs the responder in-process and the sender long-polls for the answer, as with scope amendments. It gets a real answer before the plan is committed. The cost: the answer arrives outside `prompt.Build`, so it must be rendered by a `prompt`-package function to stay inside the containment evidence.
2. **Park and resume**, like `clarification_request`. Fully inside `prompt.Build`, but it costs a whole plan re-invocation per consult and a new park state (reusing `awaiting_input` would page the captain for an agent-to-agent exchange).
3. **Asynchronous only.** Messages are delivered in the next stage's prompt or at the gate. It is cheapest, but consults degenerate into after-the-fact review, which E78's plan-fit reviewer already covers.

**D2: storage**

1. The audit chain only, with state derived by scanning. `loadClarificationAnswers` scans like this, but a scan cannot give atomic dispositions or inbox queries.
2. **The chain is authoritative, plus a derived `crew_messages` table**, the `scopeamendment` / `concern` pattern. Run-less anchors go on the account's global chain.

**D3: disagreement**

1. **A message state machine**: `open → accepted | rejected(reason)`, with rounds bounded by `AppendChainedUnderBudget`. Exhaustion writes a new escalation category that pages the captain at **every** tier.
2. Mint a `requirement` concern so the existing detector fires. This pollutes the merge gate's zero-open-concerns check and the review-bound concern fields.
3. Park a clarification to the captain.

**D4: surface and identity**

1. MCP tools first. Stage agents may not have them.
2. **REST first**, with a new `write:messages` run-token scope granted by stage type, and MCP wrappers for the operator and the operator agent.

## Recommendation

**D1 option 1 for `consult`, option 3 for `finding` and `notice`. D2 option 2. D3 option 1. D4 option 2.** Binding rules:

1. **Closed type set, standalone schema.** `consult`, `finding`, `work_request`, `notice`, `escalation`, defined in `docs/spec/crew-message-v1.schema.json` with its own package. It is not a plan `ArtifactKind`. Fields: sender role, recipient role, anchor (run, issue or decision-record id), typed payload, evidence references, response-required.
2. **Advice, never an order.** A message cannot change scope, constraints, delegation or gate outcomes. A `finding` is not a concern: it never enters the merge gate. If the captain wants one acted on, they route it explicitly, as with concerns today. An `escalation` carries `recommended_default` and `tradeoffs` (the clarification rule) and produces binding text only once the captain answers. A `work_request` files an issue through `applyAndFileWorkItem`, run-scoped, and never dispatches a run.
3. **Identity is server-derived.** The sender role comes from the token's executing stage, never from the request body. Addresses are roles, never model instances. **No message is ever delivered to an implement stage** (invariant #8). Anything an implementer should act on goes through the captain.
4. **Responders run inside fishhawkd**, reviewer-style, grounded read-only (ADR-078). Only fishhawkd-invoked responders can call `respond`. Operators rule on escalations with `write:stages`.
5. **Containment is part of the contract, not a follow-up.** Answers and delivered messages are rendered only by a `prompt`-package function inside a new `CREW MESSAGE` envelope (neutralized delimiters, `trustedMarkers` respected). The GET that returns a consult answer returns that rendered form. A resumed or retried stage folds answered consults back in through `prompt.Build`, as `mergeApprovedScopeAmendments` does. The agenteval corpus gains a crew-message channel and a seventh attack class, and `docs/compliance/prompt-injection-evidence.md` gains its row, in the same change that ships the channel.
6. **Bounded and paged.** A consult has a deadline and a per-stage cap (copy the scope-amendment window and cap). An unanswered consult times out to "no answer" and never blocks the stage. Disagreement is bounded to a small fixed number of rounds and then escalates. The new escalation category pages the captain at every tier, independent of the delegation expansion, so `low` is covered too.
7. **Audit and surfaces.** Every send and disposition is a registered audit category (`audit/categories.go`). Each new category is either added to `issuecomment` `activityCategories` or recorded as internal in `docs/issue-comment-surfaces.md`.
8. **First use: planner → historian.** The historian's answer is a deterministic precedent query (E75), so the first consult needs no model call and exercises the full channel at the lowest risk. Planner → architect follows once E78's reviewer persona exists (it needs E55's `review_conventions` grammar, E55.2 #2243).

## Decision

**Accepted (2026-09-26). Ratified by Brett (repository maintainer) on the explicit instruction "ratify both ADRs" after reviewing this proposal.** The agent records the maintainer's decision; it is not self-approval.

Adopt the Recommendation as the binding design: D1 option 1 for `consult` (synchronous, answered in-process by fishhawkd, delivered only through a `prompt`-package render inside a `CREW MESSAGE` envelope) and option 3 for `finding` / `notice`; D2 option 2 (chain authoritative plus a derived `crew_messages` table); D3 option 1 (bounded message state machine whose exhaustion pages the captain at every tier, including `low`); D4 option 2 (REST first with a server-derived sender role and a stage-typed `write:messages` scope; MCP wrappers for operators). Rules 1–8 bind the implementation, in particular: advice never an order, no delivery to implement stages, and containment evidence shipped with the channel.

**Scope of this ratification:** the architecture and invariants above. It changes no live token scope, prompt or permission until the implementation lands under E77 #3698.

## Consequences

- E77 #3698's body needs two corrections when children are filed: its "`page_human_on` at every tier" premise (true only for medium and high today), and its "MCP tools so any agent executor can take part" item (REST first, MCP for operators).
- New surfaces: a `crew_messages` table and migration, REST endpoints modelled on `/v0/runs/{run_id}/scope-amendments` (`docs/api/v0.openapi.yaml` + `v0.md`), a `write:messages` token scope, a schema with mirrors via `scripts/sync-schemas`, a prompt envelope, agenteval probes, and audit categories. This is an Auth change: the PR carries the AGENTS.md auth checklist (impact inventory; expected empty, since the scope is additive).
- Wall-clock: synchronous consults lengthen plan stages. The deadline and cap bound it, and the plan stage's own budget still applies.
- Dependencies: E75 #3696 (historian responder), E78 #3699 and E55 #2243 (architect responder).
- Not decided here: agent-to-agent transport standards (explicitly not a dependency), cross-repository messaging (N7), and any autonomy change (E82).
- Reversal: disable the `write:messages` scope and the consult endpoint. The recorded messages remain on the chain as history.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- #3698 [E77] Crew communication: typed, addressed messages on the audit chain — low confidence (0.35); shared: addressed, audit, chain, crew, messages, typed

**Parent epic suggestion**
- #3698 [E77] Crew communication: typed, addressed messages on the audit chain — medium confidence (0.45); shared: addressed, audit, chain, crew, messages, typed

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"duplicates":[{"number":3698,"url":"https://github.com/kuhlman-labs/fishhawk/issues/3698","title":"[E77] Crew communication: typed, addressed messages on the audit chain","score":0.35294117647058826,"confidence":"low","basis":"addressed, audit, chain, crew, messages, typed","closed":false}],"epic_suggestion":{"number":3698,"url":"https://github.com/kuhlman-labs/fishhawk/issues/3698","title":"[E77] Crew communication: typed, addressed messages on the audit chain","score":0.4529411764705883,"confidence":"medium","basis":"addressed, audit, chain, crew, messages, typed"},"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2586} -->
