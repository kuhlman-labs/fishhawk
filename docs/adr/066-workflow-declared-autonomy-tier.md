---
id: ADR-066
title: "Workflow-declared autonomy tier and path-scoped control surface"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2209
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-066: Workflow-declared autonomy tier and path-scoped control surface

## Context

`.fishhawk/workflows.yaml` is positioned as the operator's ultimate control surface, but an operator cannot read the autonomy posture out of it. A 2026-07-26 cold-read review of the spec from a non-technical-operator perspective surfaced five gaps.

**1. Autonomy is derivable but never declared.** The answer to "how much can the agent do without me?" is spread across seven fields: `drive`, `operator_agent.may_*`, `operator_agent.must_page_human`, per-stage `gates[]`, per-stage `reviewers` counts, `constraints[]`, and `budget.enforcement`. The three shipped presets (`workflow-preset-{low,medium,high}.yaml`, ADR-048) differ ONLY in the `operator_agent` block — yet nothing in a generated file states the tier. `docs/METHODOLOGY.md`'s tier table is a commitment in Markdown, not a declared, greppable, machine-checkable field.

**2. Reviewer authority is derived from integers, not declared.** The `reviewers_config` authority table is `agents>0 && human==0 → gating`; `agents>0 && human>0 → advisory`. Adding a human reviewer silently DEMOTES agent reviewers from blocking to advisory. The rule lives only in the schema's description prose; nobody will infer it from two counts.

**3. Workflow selection is out-of-band and unconstrained.** The operator passes `workflow_id` to `start_run`. Nothing in the spec says which changes may use which workflow, so a cryptographic change can be routed through `routine_change` — whose gate is degenerate (`type: check`) and which permits agent merge. METHODOLOGY names crypto / policy / audit-integrity as human-led; the spec cannot enforce it.

**4. Path containment is post-hoc only.** `allowed_paths` / `forbidden_paths` are diff constraints evaluated after the agent has already read and written. There is no preventive scoping and no per-path escalation ("changes under `**/crypto/**` require two approvals from security"). `egress` (ADR-050 / v1.3) is the only preventive capability control and the validator restricts it to acceptance stages (`backend/internal/spec/validate.go:261`).

**5. A second consumer already needs this.** ADR-065 (#2161) records the operator requirement that a grooming agent's autonomy be "tunable in the workflow declaration itself." That is the same missing primitive, requested independently for a non-code-change workflow.

Comparison anchor: GitHub Actions and GitLab CI both lead with `on:` / `rules:` (when this runs) and both ship a `permissions:` block. Fishhawk has neither, while claiming the workflow file as the governance surface.

## Options

1. **Status quo plus documentation.** Explain the derived authority table and the tier mapping harder in `docs/`. Rejected: the tier stays underivable from the file an operator actually reads and edits, and workflow routing stays unenforceable by construction.

2. **Declared autonomy tier, explicit reviewer authority, and declarative path predicates.**
   - `autonomy: low | medium | high` at workflow level, expanding to the ADR-040 `operator_agent` knob preset for that tier; an explicit `operator_agent` block overrides per-knob, so nothing currently expressible becomes inexpressible.
   - `reviewers.authority: advisory | gating` declared explicitly, with the count-derived rule retained as the default for back-compat.
   - `applies_to:` per workflow (path globs / labels / change kind), making workflow selection a reviewable spec decision; `start_run` validates the requested `workflow_id` against it and fails closed on a mismatch.
   - `escalations:` — a path predicate that RAISES requirements for a matching diff (approval `count`, `member_of`, an autonomy floor), turning METHODOLOGY's tier table into an enforced control.
   - Generalize `egress` off the acceptance-only binding and add a per-stage `permissions:` block (network, write scope, shell posture) — preventive rather than post-hoc.
   - ONE shared path-predicate primitive backing `applies_to`, `escalations`, and the conventions `applies_to` in the companion review-conventions ADR. Three surfaces, one implementation.

3. **An expression language (`${{ }}`-style) for conditional policy.** Rejected: a policy file that computes its own policy is not statically reviewable, and static reviewability is the property that makes a governance artifact auditable. It is also the single largest reason Actions workflows become unreadable. The conditionality we need is declarative predicates, not computation.

4. **Enforce tiers outside the spec, in an org policy service.** Rejected for alpha: introduces a second source of truth for governance and a deployment dependency, when the spec is already the artifact the audit chain references.

## Recommendation

Option 2, sequenced in two waves rather than shipped as one block.

**Wave 1 — declaration (cheap, additive, high value).** `autonomy:` and `reviewers.authority:`. Both are pure declaration over machinery that already exists, both are additive, and together they answer the operator's primary question from the top of the file. `autonomy:` also collapses the preset library from three near-identical documents to a tier plus deltas, and supplies ADR-065 the primitive it asks for.

**Wave 2 — routing and containment.** `applies_to:`, `escalations:`, `permissions:` / generalized `egress`. Larger, and one honest constraint applies: real preventive enforcement of write scope and shell posture depends on the container sandbox in ADR-063 (#2127). Until that lands, `permissions:` must be declared-and-audited, explicitly NOT claimed as enforced — the failure mode to avoid is an operator believing a spec field contains an agent that it merely describes.

Build the path predicate once, in wave 2, and have the review-conventions ADR consume it rather than growing a parallel matcher.

Interaction with the grammar-consolidation ADR filed alongside this one: if that consolidation is ratified, these fields should be designed INTO the consolidated grammar rather than bolted onto the current major and re-cut immediately after.

## Decision

**Accepted (2026-07-26).** Adopt Option 2. Four forks were settled at ratification; where they refine the Recommendation, **these govern**.

### 1. One autonomy grammar: action-class matrix with a tier shorthand

The competing vocabularies are merged rather than coexisting. Each **action class** declares a `mode` and, where the backend needs one to make `auto` safe, the closed **condition** under which it may act:

```yaml
autonomy: medium              # shorthand; expands to the matrix below
actions:                      # explicit per-class overrides
  approve: {mode: auto, when: clean_dual_approval}
  fixup:   {mode: auto, when: convergent_concerns, min_severity: medium}
  waive:   {mode: gated}
  retry:   {mode: auto, when: infra_flake}
  merge:   {mode: gated}
  page_human_on: [gating_reviewer_reject, plan_rejection, scope_amendment, ...]
```

**Modes** — `auto`: act without paging, subject to `when`. `gated`: park and wait for the human (today's fail-closed default). `report`: surface the proposal without acting or blocking. **`report` is genuinely new** — the current `may_*` knobs cannot express it, because knob-absence means page. It is what ADR-065's grooming workflow needs for its `scoping` class, and it is useful for code-change workflows too.

**Conditions** (`when`) keep the ADR-040 property that made delegation safe: every condition is a closed, backend-evaluable predicate answerable from run state. This is not a loosening — `mode: auto` with no valid condition for that action class is rejected.

This **supersedes** the `operator_agent.may_*` shape. `must_page_human` becomes `actions.page_human_on`. Grooming (ADR-065 / #2161) uses the same grammar with its own action classes (`hygiene`, `ordering`, `dedup`, `scoping`), so there is exactly one place an operator declares autonomy, for any workflow type.

### 2. Sequencing correction discovered at ratification — the autonomy grammar belongs INSIDE E52

Replacing `operator_agent.may_*` with the `actions:` matrix is a **breaking** grammar change. E52 (#2212) is currently scoped to carry `operator_agent` forward into `workflow-v2` unchanged, and its codemod child (E52.8 / #2220) would translate v1 `operator_agent` into v2 `operator_agent`. If this ADR's grammar then lands afterward, **v2 ships with a grammar we have already decided to replace**, and the codemod is written twice.

So this ADR's fields split by breaking-ness:

| Field | Nature on v2 | Where it lands |
|---|---|---|
| `autonomy:` + `actions:` matrix (replacing `operator_agent`) | **Breaking** | **E52**, as a new child in wave 2, before the codemod |
| `reviewers.authority:` | Additive (count-derived rule stays the default) | This ADR's epic |
| `applies_to:` | Additive | This ADR's epic |
| `escalations:` | Additive | This ADR's epic |
| `permissions:` | Additive | This ADR's epic |

Both waves are in scope as ratified — the split is about which schema major absorbs each field, not about deferring any of them.

### 3. `permissions:` ships declaration-only, explicitly labeled

The field is validated, audited and surfaced, and its schema description and documentation must state plainly: **declared and audited, NOT enforced until E51 #2133 (ADR-063 container isolation) lands.** The known risk is that an operator reads it as containment anyway; the mitigation is that the non-enforcement is stated in the field itself, not only in an ADR. Do not describe it as a security control until E51 lands.

### 4. `applies_to` fails closed at `start_run`

A run whose requested `workflow_id` does not satisfy that workflow's `applies_to` predicate is **rejected**, not warned. This is the control the ADR exists for — it makes routing a crypto change through a high-autonomy workflow a spec violation rather than an operator habit, and turns METHODOLOGY's tier table into something enforced.

Expect friction the first time a declaration is narrower than actual practice. That friction is the feature working; the fix is amending the declaration, which is a reviewable change to a governance file.

### Also settled by ADR-067 (#2210) and binding here

The path predicate **cannot be diff-only**: v2 decouples stage types from "produces a diff", so a non-code workflow uses the same stage types and must be routable by the same mechanism. `applies_to` needs a non-diff trigger form (scheduled / on-demand) from the start.

Named approver: repository maintainer (human).

## Consequences

An operator can answer the autonomy question from one declared line instead of reconstructing it from seven fields. METHODOLOGY's autonomy tiers stop being a Markdown promise and become machine-checkable, and misrouting a change to a high-autonomy workflow becomes a spec-level violation rather than an operator habit. ADR-065's grooming agent gets the declaration primitive it requires without inventing its own. The preset library collapses to a base plus tier deltas.

Costs and risks: `permissions:` overlaps ADR-063's containment work and must not advertise enforcement it does not have — declaration-only must be stated in the field's own documentation, not just the ADR. A new path-predicate primitive becomes a shared dependency of three surfaces and needs a single implementation with one glob semantics (`**` crossing `/`, matching the existing `test_conventions` doublestar convention) rather than three subtly different matchers. `applies_to` introduces a new fail-closed rejection path at `start_run` that will surface as friction the first time an operator's routing declaration is narrower than their actual practice.
