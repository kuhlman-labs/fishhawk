---
id: ADR-040
title: "Operator agent as a product-defined role contract: versioned playbook, gate-delegation knobs, thin-by-design"
status: accepted
date: 2026-06-12
issue: https://github.com/kuhlman-labs/fishhawk/issues/997
supersedes: []
superseded_by: []
applies_to: ["docs/spec/operator-role.schema.json", ".fishhawk/operator.yaml"]
---

# ADR-040: Operator agent as a product-defined role contract: versioned playbook, gate-delegation knobs, thin-by-design

## Context

After ~17 operator-driven dogfood loops (2026-06-09 → 06-11), the operator role — the medium between human operators and the implement/review agents — is performed ad hoc by whichever agent session is driving, with the accumulated playbook living in that agent's private project memory. The playbook content proved load-bearing (await-both-heterogeneous-reviewers before approving; arbitrate split verdicts by verifying the claim in code; override-in-first-comment after #986; post-merge verification; follow-up filing conventions; retro cadence), but it is unversioned, unauditable, unportable across repos, and dies with the session. Fishhawk is meant to run across multiple repos and work-management platforms; the operator behavior must be consistent everywhere.

Three observations from the lived role shape the options:

1. Operator actions decompose cleanly into three buckets: **mechanical advancement** (run_stage → await → merge → post-merge; ~70% of calls), **policy-following judgment** (clean dual-approve → approve; convergent reviewer concerns → fixup; infra flake → retry), and **genuine human calls** (policy overrides, exception renewals, requirement-authorship arbitration — e.g. the #969 "into the trace" amendment).
2. Every operator workaround eventually became product (hand-polls → #962; branch rituals → #967; salvage merges → #978/#989). An operator agent with a thick bag of tricks would freeze that conversion.
3. The human's effective interface was the operator's *distilled* context at gates (split-verdict summaries, provenance notes), not raw run state.

## Options

**A. Freestanding operator agent with its own memory** (status quo, formalized). An agent instance per deployment carries the playbook in private memory.
- Cheap to start; matches today's behavior.
- Unauditable, divergent across repos/instances, knowledge dies with sessions, anti-thesis of the product's spec-versioned approach.

**B. Product-defined operator ROLE CONTRACT (recommended).** The product ships a versioned operator role spec — prompt, tool grants, playbook, escalation rules — as a first-class artifact (the same move `.fishhawk/workflows.yaml` made for workflows), with per-repo overlays for local conventions. Any harness (Claude, Codex, a scheduler) instantiates the role.
- The workflow spec gains explicit **gate-delegation knobs**, e.g.:
  `operator_agent: {may_approve: clean_dual_approval, may_arbitrate: advisory_concerns, may_retry: infra_flake, must_page_human: [rejections, overrides, exceptions, policy_overrides]}`
  — bucket-2 judgment becomes auditable, tunable per autonomy tier (METHODOLOGY.md), and identical across repos. ADR-027 authority semantics unchanged: the operator agent acts within delegated bounds and PAGES the human with distilled context otherwise.
- The role stays **thin** by design: judgment patterns live in the role spec; *procedure* lives in the product. Prerequisites that enforce thinness: drive mode (backend advances mechanical transitions, #996 theme 1) and server-suggested next actions (generalized review_action_hint, #996 theme 3). Operator memory accumulated in violation of thinness is a signal to file product issues, not extend the playbook.
- Per-repo work-management config (`.fishhawk/work_management`, #996 theme 4) supplies filing conventions so they never live in agent memory.

**C. Fully automated operator (no agent).** Encode bucket 2 as backend policy rules.
- Loses the judgment quality that made arbitration work (reading reviewer claims against code, composing binding amendments); premature given how often verdict arbitration required real reasoning this session.

## Recommendation

Option B. Target topology: **human ↔ operator agent (role-contract instance, paged at gates with distilled context) ↔ fishhawkd drive mode (mechanical orchestration) ↔ implement/review agents** — every boundary a spec surface, every hop audited, nothing load-bearing in session memory.

Sequencing: (1) drive mode + next-action hints (prerequisites, tracked in #996); (2) extract the session playbook into a versioned operator role spec (the 2026-06 dogfood memory file is the first draft); (3) gate-delegation knobs in the workflow schema (additive, schema-change checklist applies); (4) instantiate and dogfood the role on this repo before generalizing.

## Decision

**Option B adopted** (ratified by the human operator 2026-06-12; full rationale in the [draft comment](https://github.com/kuhlman-labs/fishhawk/issues/997#issuecomment-4690566165)):

1. **Artifact topology**: the base operator role spec is a product artifact — canonical JSON Schema `docs/spec/operator-role.schema.json` (`operator-role-v0`) + reference doc + shipped versioned default. Per-repo overlay `.fishhawk/operator.yaml` may only select knob presets, local conventions, and the work-management config pointer; it structurally cannot add playbook procedure (thinness rule made structural — per-repo procedure is a product gap to file).
2. **Delegation knobs live in the workflow spec** (optional `operator_agent` block, workflow-level with per-gate override) with a closed backend-evaluable condition enum v0: `clean_dual_approval`, `convergent_concerns`, `solo_low`, `infra_flake`, `gates_resolved_ci_green`, plus `must_page_human: [reviewer_reject, plan_rejection, scope_amendment, budget_override, policy_override, exception_request, requirement_arbitration]`. Fail-closed: anything not delegated pages the human. ADR-027 authority semantics unchanged.
3. **Prerequisites relaxed to a parallel track**: drive mode (#1023) and generalized next actions (#1024) do not gate the role contract; they land in parallel and progressively thin the role.
4. **Identity + paging v0**: operator-agent actions run under a distinct token subject (`operator-agent/<role-spec-version>`); paging v0 is the driving session surface with issue-comment fallback (general channel answer deferred to ADR-015/#932).

## Consequences

1. New spec surface `operator-role-v0` (schema-sync enforced); workflow schema gains the optional `operator_agent` block (additive; schema-change checklist applies; no required promotion planned).
2. METHODOLOGY.md autonomy tiers map to knob presets (low = empty block, everything pages; high = full v0 enum delegated).
3. Implementation children: #1025 (role-spec schema + shipped default + validation), #1026 (operator_agent knobs + condition evaluation + audit attribution), #1027 (operator token subject), #1028 (dogfood instantiation on this repo). Parallel: #1023 (drive mode), #1024 (next actions), #1005/#1012 (work-management config).
4. The 2026-06 dogfood session playbook is superseded by the versioned spec once #1028 completes; agent memory entries become pointers to the spec.
5. Accepted risks: the v0 enum may prove too coarse (a page-the-human event rubber-stamped identically 5+ times is a candidate new enum value); role-spec versioning follows major-version rules (breaking behavior change bumps to operator-role-v1).
