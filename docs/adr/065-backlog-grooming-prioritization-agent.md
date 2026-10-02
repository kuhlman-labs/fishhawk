---
id: ADR-065
title: "Backlog grooming / prioritization agent with workflow-declared tunable autonomy"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2161
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-065: Backlog grooming / prioritization agent with workflow-declared tunable autonomy

## Context

Fishhawk owns the mechanical layers of work management — filing conventions (`work-management-v0`, #1005), board placement, and (pending ADR-064) board-as-input/projection — but the judgment layer above them is missing: deciding what to work next and whether the backlog coheres with the product's direction. The friction is recurring and quantified: the 2026-07-23 triage session behind ADR-064 (~150 hand-rolled mutations), and every campaign batch to date being founder-hand-picked. The gap compounds because Fishhawk itself generates backlog inflow faster than humans groom it: deferred review concerns, product reports (#1006), acceptance/incident intake (E35/ADR-053), decomposition follow-ups.

Existing machinery this composes with: the `workmgmt` provider abstraction (currently create-only), ADR-064's declarative-input/derived-projection split (agents forbidden from using the board as a coordination bus), the #2051 explicit-issue-list campaign source, issue-level `depends_on` edges feeding the campaign wave DAG (ADR-047), `fishhawk_draft_epic`, and the plan-gate propose/decide pattern.

Operator requirement (2026-07-24): the grooming agent's autonomy must be tunable **in the workflow declaration itself** — that is how a human operator expresses control. Some repos want a fully autonomous groomer, others want it human-led; both need first-class knobs, not a product-wide fixed posture.

Timing pressure: E45's forge-agnostic work-item abstraction and the ADR-057 provider model are being shaped now. The read/list/update provider surface a groomer needs is cheap to reserve in those interfaces today and a refactor later (same lesson as the ADR-057 provider discriminator and the ADR-064 board seam).

## Options

1. **Status quo: ad-hoc operator-prompted grooming sessions.** Rejected direction: unrepeatable, unaudited, no vision anchor (judgment drifts per invocation), and the friction recurs for every tenant — this is a friction class the product exists to absorb.

2. **Fixed-autonomy grooming service** (e.g. always report-only, or always auto-apply). Rejected direction: violates the operator requirement above — repos differ in how much control they want to delegate, and a hardcoded posture forces the most conservative repo's ceiling on everyone or the most permissive repo's floor.

3. **Grooming as a first-class workflow with charter-anchored scoring and workflow-declared per-action-class autonomy knobs.** A `backlog_grooming` workflow (scheduled or on-demand) whose run produces a structured grooming-report artifact; a checked-in charter (phase themes, non-goals, prioritization rubric) anchors the scoring so rankings cite rubric lines instead of per-run judgment; an additive `workflow-v1.x` block declares, per action class, whether the groomer reports, gates, or auto-applies.

4. **Intake-only triage** (event-driven micro-groom on each `fishhawk_file_issue`: dup check, epic suggestion, initial score — no periodic sweep). Partial: valuable and cheap, but does not address prioritization, ordering, or vision drift across the existing backlog. Subsumed as one component of option 3.

## Recommendation

Option 3, with option 4's intake hook as a component. Key elements:

- **Charter artifact** (checked-in; location TBD — candidate: referenced from the work-management config via an additive optional pointer field): north star, current-phase themes, explicit non-goals, prioritization rubric (value / risk / dependency-unblocking / staleness). The groomer scores against it and every proposed ranking cites the rubric line that justifies it — the agent surfaces deviations from a stated vision rather than inventing priorities.

- **Grooming-report artifact** produced by the run: proposed priority order with per-item scores, duplicate candidates, hygiene defects (missing label namespaces, absent Done-means, unlinked parent epics, `boarded:false` strays), suggested `depends_on` edges (improves campaign wave-DAG quality), vision-drift flags, decomposition suggestions (executed via `fishhawk_draft_epic`).

- **Autonomy knobs in the workflow declaration** (additive `workflow-v1.x`; the workflow spec is the operator's control surface). Per **action class**, one of `report` | `gated` | `auto`:
  - `hygiene` — labels, fields, boarding, epic links (objective, reversible)
  - `ordering` — priority scores, queue order
  - `dedup` — flag duplicates vs close-as-duplicate
  - `scoping` — decompose, icebox, close
  A fully autonomous repo sets `auto` across classes; a human-led repo sets `report`/`gated`. Shipped defaults are conservative (`hygiene: auto`-eligible, everything else `gated` or `report`); `auto` for destructive actions (closing) is opt-in and never a default. Nothing auto-closes under the default matrix.

- **Gate mechanics reuse the plan-gate pattern**: report artifact → operator approves (or rejects with a reason that feeds the next grooming run) → apply layer executes the approved mutations through the `workmgmt` provider, every mutation audited. This is what makes agent-authored ordering compatible with ADR-064's board boundary: the groomer's writes are gate-ratified selection-time human intent, never in-flight coordination; under `auto`, the ratification is the operator's standing declaration in the workflow config, and the audit chain still records every mutation.

- **Campaign feed**: the approved priority order becomes the Up Next queue / #2051 explicit-issue-list campaign source — groom → ratify → campaign works the queue.

- **Churn guard**: minimal-diff discipline — propose only changes crossing a declared threshold; idempotent run-over-run, so repeated runs on an unchanged backlog propose nothing.

- **Sequencing**: implementation is post-alpha. Near-term actions are reserving the seams while E45/ADR-057 are still being shaped: (a) read/list/update on the work-item provider abstraction (today's surface is create-only), (b) an additive optional charter-pointer field in `work-management-v0`, (c) the per-forge capability caveats from ADR-064 (user-owned Projects v2 token classes, 100-item board caps, GitLab label-driven boards) documented as applying doubly to a reader — the groomer enumerates via search/issue queries, never board item lists.

## Decision

**Accepted (2026-07-26).** Adopt Option 3 (grooming as a first-class workflow with charter-anchored scoring and workflow-declared autonomy) with Option 4's intake hook as a component. **Implementation is deferred post-alpha**, per this ADR's own sequencing; what is ratified now is the design and the seams. Three forks were settled; where they refine the Recommendation, **these govern**.

### 0. Findings: two of the three near-term seam actions are already filed, and two open questions are already resolved

The Recommendation's near-term list was (a) provider read/list, (b) a charter-pointer field, (c) per-forge capability caveats documented as applying doubly to a reader. Ratifying ADR-064 (#2139) filed **(a) as E45.23 #2230** — carrying the enumerate-via-search-queries-never-board-item-lists requirement, with this groomer named as the downstream consumer — and **(c) as E45.24 #2231**. Only **(b)** remained.

Separately, two questions raised against this ADR are resolved:

- **Autonomy vocabulary** — settled by ADR-066 (#2209) in this ADR's favour. The `report | gated | auto` shape won and became the product-wide grammar (E52.10 #2222); the action-class set is **extensible per workflow type**, so `hygiene` / `ordering` / `dedup` / `scoping` are this workflow's classes under the same grammar every workflow uses. There is no second config surface for grooming autonomy.
- **Board reads** — settled by ADR-064. The groomer reads the backlog through the **server-side provider seam** under a gate, never through an agent-facing board tool; no board-read capability is exposed to agents via MCP.

### 1. Scope now: the charter-pointer seam only

File the additive optional charter-pointer field in `work-management-v0` and stop. The groomer workflow, grooming-report artifact, scoring, apply layer, campaign feed and intake hook are **deferred post-alpha**, consistent with this ADR's sequencing and with five epics already open (E52, E53, E51, E35, E44).

### 2. The charter reuses ADR-068's repo-declared-Markdown mechanism

The charter is a checked-in Markdown document declared by path in `work-management-v0` — **the same shape as ADR-068's (#2211) review conventions**: resolved from the **base ref** (so an agent cannot edit the charter that constrains it in the same change), **injected server-side** (so every adapter sees identical content regardless of repo access), **content-hash attributed** in the audit trail (so a ranking is attributable to a specific charter revision), and **size-capped**.

This is deliberate reuse, not coincidence: both are repo-authored prose documents that shape agent judgment and must be tamper-evident and attributable. **Build the injection machinery once, with two consumers.**

**Dependency created:** the charter's *mechanism* now depends on ADR-068 (#2211), which is not yet ratified. The *pointer field* is independently additive and can land first; the injection machinery follows ADR-068's implementation. This materially strengthens ADR-068 — it has two consumers, not one.

Rejected: a structured-YAML rubric (turns judgment into arithmetic and cannot carry the narrative north star), and no charter at all (this ADR's own Consequences note that without an anchor the groomer must degrade to report-only).

### 3. The grooming report is a new `grooming_report` artifact kind

A fifth artifact alongside `plan` / `pull_request` / `deployment` / `acceptance`, with its own schema. It rides the existing produces / persistence / audit machinery, and the propose → operator-decides → apply gate pattern works unchanged.

Rejected: reusing the `standard_v1` plan artifact. Its shape (`scope.files`, approach steps, verification, rollback) fits a code change; nearly every field would be vestigial for a ranked backlog, and misusing a schema carries its own legibility cost — the exact problem the workflow-spec consolidation was ratified to fix.

This is decided **now** even though implementation is deferred, so the seams being reserved are shaped against a known consumer.

Named approver: repository maintainer (human).

## Consequences

The backlog stays decision-ready as agent-generated inflow grows, and the recurring hand-triage friction class is absorbed by the product. The operator's control posture becomes an explicit, reviewable, per-repo declaration in the workflow spec rather than an implicit product default — repos can run the spectrum from report-only to fully autonomous. The `workmgmt` provider abstraction must grow read/list/update (E45 seam). A charter artifact becomes effectively required for repos enabling grooming beyond hygiene — without it the groomer has no anchor and must degrade to report-only. Grooming mutations enter the audit chain; the board remains a projection/selection surface per ADR-064. Risks: prioritization quality is bounded by the charter's specificity (mitigated by the gate and by rubric-citation requirements); report churn can train rubber-stamping (mitigated by the threshold/idempotence discipline).
