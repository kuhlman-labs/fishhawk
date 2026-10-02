---
id: ADR-067
title: "Workflow spec grammar consolidation: break the duplicate surfaces and add reuse before the first external consumer"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2210
supersedes: []
superseded_by: []
applies_to: ["docs/spec/workflow-v2.schema.json"]
---

# ADR-067: Workflow spec grammar consolidation: break the duplicate surfaces and add reuse before the first external consumer

## Context

The repo is pre-alpha with no downstream consumers of `.fishhawk/workflows.yaml` — this repo is the only executor. The schema-evolution policy (`docs/spec/workflow-v0.md` §Schema evolution) commits to never breaking a major in place, so every duplicate or legacy surface retained today is carried forever. The window to consolidate closes at the first external consumer.

A 2026-07-26 cold-read review of the spec identified what a consolidation would remove:

**Duplicate and legacy surfaces in the current major.**
- `approvers` (GitHub-handle role allow-list, plus the top-level `roles` map) versus `approvals` (forge-neutral predicate, ADR-055 / #1707) coexist as a schema-level exclusive-or. Two ways to express one concept, one of which is forge-coupled and which ADR-055 already superseded as the preset default.
- `must_page_human`'s bare `reviewer_reject` is retained for back-compat and silently resolves to the gating sense, alongside the explicit `advisory_reviewer_reject` / `gating_reviewer_reject` tokens (v0.7 / #1378).
- `reviewers.agent` (integer count) versus `reviewers.agents[]` (list), where a non-empty list supersedes the integer.

**No reuse primitives at all.** Actions has reusable workflows, composite actions, and `defaults:`; GitLab has `extends:`, `include:`, `!reference`, and `default:`. Fishhawk has none. The three presets are near-copies of one another, and this repo's own `.fishhawk/workflows.yaml` repeats a byte-identical `reviewers` block twice. Copy-paste is precisely the drift the schema-sync gate exists to catch after the fact.

**The version enum is an operator trap.** `1.0`…`1.6`, each minor named after an epic, minor not routing-significant — yet an operator must know `egress` needs ≥1.3 and `diff_coverage` ≥1.6. Combined with `additionalProperties: false` on most objects, this generates a recurring "my valid field was rejected" class of error.

**Units and shapes are inconsistent.** Time is spelled three ways in one file: `policy.max_stage_runtime: "30m"`, `executor.timeout: 10m`, `verify.timeout: "15m"`, and `budget.max_runtime_minutes: 15`. `budget.max_tokens: 500000` puts an agent implementation detail into an operator-facing file that an operator cannot price — while `budgets[].limit_usd` already proves the right unit exists. `constraints:` is a list of single-key maps (`maxProperties: 1`) where a plain object would read directly. `drive: true` names a mechanism rather than an outcome. `inputs[].from_stage` artifact wiring is largely derivable from the closed stage-type set.

**Legibility, measured.** This repo's own `.fishhawk/workflows.yaml` carries 142 comment lines against 203 config lines, and the comments are predominantly issue and ADR references — an engineering changelog living inside a policy file. The shipped presets carry the same style at roughly 35% comments. A new operator reading a preset learns Fishhawk's issue history before they learn their own policy. That ratio is a symptom: fields whose semantics need a paragraph of justification are not self-evident enough.

## Options

1. **Freeze the current major, additive-only forever.** Rejected: locks in three duplicate surfaces and the absence of any reuse primitive before a single external consumer exists — paying the maximum long-term cost to avoid the minimum short-term one.

2. **A consolidating major bump, executed now.**
   - *Remove:* `approvers` and the top-level `roles` map (keep `approvals`); the bare `reviewer_reject` token (keep the two explicit classes); the `reviewers.agent` integer (keep `agents[]`).
   - *Add reuse:* `defaults:` at file and workflow level for executor / reviewers / budget, and `extends:` on a workflow.
   - *Collapse versioning:* `version: "2"`, with field-presence validation replacing minor-gated acceptance.
   - *Unify units:* one duration form everywhere (the Go duration string already used by `max_stage_runtime`); express stage budgets in USD, with tokens demoted to an optional secondary lever.
   - *Reshape for reading:* `constraints:` becomes an object; `drive:` becomes `auto_advance:`; `needs: [<stage_id>]` as shorthand for the common artifact wiring.
   - *Preset hygiene:* policy-only comments in the shipped presets, with rationale relocated to `docs/spec/`.

3. **Break selectively — cleanup only, no reuse primitives.** Rejected as a half-measure: reuse is the change that eliminates the copy-paste generating the drift, and a second breaking bump later to add it costs more than including it now.

4. **Add `include:` for org-level baseline workflow files.** Deferred rather than rejected — the natural home for a platform-team baseline ("no repo may declare `autonomy: high`") but it depends on multi-tenancy (ADR-057) to decide where a baseline lives and who owns it. Reserve the seam; do not build it here.

## Recommendation

Option 2, ratified and executed BEFORE the first external consumer and BEFORE the ADR-066 (#2209) control surface lands. The ordering matters: if the autonomy tier, reviewer authority, routing, escalations, and permissions fields are bolted onto the current major, they get re-cut immediately afterward and the consolidation pays for two migrations instead of one. Design them into the consolidated grammar.

Old specs stay readable forever under the existing evolution policy — v0 and v1 schemas remain compiled and routed, and audit-log runs are unaffected. Migration for this repo is a `fishhawk` CLI codemod plus regenerated presets.

Defer `include:` (option 4) to the multi-tenancy work, and keep the reserved seam explicit so it is not re-litigated.

## Decision

**Accepted (2026-07-26).** Adopt Option 2 — a consolidating major bump (`workflow-v2`), executed now, at full scope. Three forks were settled at ratification; where they refine the Recommendation, **these govern**.

### 1. Scope: full consolidation

- **Remove** all three duplicate surfaces: `approvers` + the top-level `roles` map (keep `approvals`); the bare `reviewer_reject` page-event token (keep `advisory_reviewer_reject` / `gating_reviewer_reject`); the `reviewers.agent` integer (keep `agents[]`).
- **Add reuse:** `defaults:` at file and workflow level, and `extends:` on a workflow.
- **Collapse versioning** to `version: "2"`, with field-presence validation replacing minor-gated acceptance.
- **Unify units:** one duration form throughout (the Go duration string); stage budgets expressed in USD, with `max_tokens` demoted to an optional secondary lever.
- **Reshape for reading:** `constraints:` becomes an object; `drive:` becomes `auto_advance:`; `needs: [<stage_id>]` as shorthand for the common artifact wiring.
- **Preset hygiene:** policy-only comments in the shipped presets; rationale relocated to `docs/spec/`.

### 2. Stage types: generalize the existing set — do not grow it, do not add a `kind:` discriminator

Stage `type` stays a closed set, but its members are **decoupled from "produces a diff"**. `plan` / `implement` / `review` already generalize to propose / apply / gate: a grooming workflow proposes a report, applies approved mutations, and gates on approval, using the same three types. Constraint validity binds to **what a stage actually produces**, which is the same type↔constraint binding the semantic validator already enforces for deploy versus non-deploy — extended rather than replaced. The post-hoc diff constraints (`max_files_changed`, `allowed_paths`, `forbidden_paths`, `required_outcomes`, `diff_coverage`) are valid only on a stage that produces a diff.

This settles the question raised against ADR-065 (#2161): a `backlog_grooming` workflow is expressible in the spec without new stage types and without a second place to declare autonomy.

**Sub-decision left to the epic, with a recommendation: do NOT rename the types.** The propose/apply/gate mapping above is conceptual. Renaming `plan` / `implement` / `review` would churn audit kinds, MCP tool semantics, prompt builders, run-state machinery, and every doc, for no operator-legibility gain — the existing names read fine and are widely understood. Recommend retaining them and changing only their binding semantics. Raise it explicitly in the epic if the implementer disagrees.

### 3. Sequencing: this consolidation BLOCKS ADR-066's control-surface fields

ADR-066 (#2209) — `autonomy:`, `reviewers.authority`, `applies_to:`, `escalations:`, `permissions:` — lands **on the consolidated major**, not additively on v1.x. One migration, one schema-major bump, one set of mirrors and routing entries, and 066's fields are named and shaped against a clean grammar rather than bolted on and re-cut.

### 4. `include:` remains deferred

Org-level baseline workflow files stay out of scope, pending multi-tenancy (ADR-057 / E44) deciding where a baseline lives and who owns it. The seam is reserved deliberately; do not re-litigate it in the epic.

Named approver: repository maintainer (human).

## Consequences

One breaking bump, paid at the cheapest moment it will ever cost. The preset library becomes a base plus tier deltas rather than three maintained near-copies; the version enum stops being an operator trap; the spec loses two forge-coupled and three redundant surfaces before anyone depends on them.

Mechanical cost, per the ADR-046 new-major checklist in `AGENTS.md`: a canonical `docs/spec/workflow-v2.schema.json`, `scripts/sync-schemas` mirroring into `backend/internal/spec/schemas/` and `cli/internal/spec/schemas/`, an `embeddedSchemas` routing entry in BOTH `backend/internal/spec/parse.go` and `cli/internal/spec/spec.go`, an `EmbeddedSchemaHashV2()` plus the `workflow-v2` `/healthz` schemas-map entry, and a new self-referential pattern in `backend/internal/server/surface_sweep.go`. The `version:` major bump additionally fires the `scripts/dev` schema-major reconnect banner (#1422) — which is the intended loud signal, not a side effect.

Risks: a codemod that silently mistranslates an `approvers` allow-list into an `approvals` predicate would quietly change who can approve, so the migration needs an explicit before/after approval-eligibility diff rather than a blind rewrite. Removing minor-gated validation shifts a class of errors from "rejected at parse" to "rejected at field validation", which must produce equally actionable messages.
