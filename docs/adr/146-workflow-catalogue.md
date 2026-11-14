---
id: ADR-146
title: "The workflow catalogue: five families, presets, and cross-cutting controls"
status: accepted
date: 2026-11-14
issue: https://github.com/kuhlman-labs/fishhawk/issues/5260
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-146: The workflow catalogue: five families, presets, and cross-cutting controls

**Status: accepted (ratified 2026-11-14).**

## Context

Prepared at the Captain's request on 2026-11-14. Repository citations are to forge main at `6ff56c32`.

The evidence is out of tree, in `~/Projects/fishhawk-workflow-catalogue-review/`:
- the first officer's proposal (`report.md`), from a backlog and ADR sweep and from run and PR data for 2026-09-15 to 11-14;
- two independent reviews of it (`review-fable.md`, `review-astra.md`);
- their synthesis (`synthesis.md`).

Run counts come from paged list reads and are approximate.

**What the product ships.** The preset library is one workflow: "a single `feature_change` workflow" in each document (`docs/spec/workflow-preset.md:18`), across 3 tiers × 2 shapes, six documents (`:373`). `fishhawk init` writes a preset whole, because `extends:` is same-document only and cross-file `include:` is deferred (`:375-378`; ADR-067, `docs/adr/067-workflow-spec-grammar-consolidation.md:47`). The scope rule is "Different work types use different workflows" with no conditionals (`docs/MVP_SPEC.md:92`). The five stage types already read generally: propose, apply, gate, delegate, verdict (`docs/spec/workflow-v2.md:479-487`).

**What this repository declares.** It declares seven workflows: `feature_change`, `hotfix_change`, `routine_change`, `human_led_change`, `release`, `backlog_grooming` and `upkeep_scan` (`.fishhawk/workflows.yaml:464`, `:905`, `:1047`, `:1194`, `:1258`, `:273`, `:416`). The spec also ships examples for milestone scoping, user-report scanning and the hotfix preset (`docs/spec/examples/`; `docs/spec/workflow-v2.md:1433`).

**How they are used** (60 days):
- `feature_change` carries about 1,060 runs.
- `routine_change` has none since 08-12.
- `human_led_change` has two, and neither completed.
- `release` has none, because the product has not yet reached a release.
- Six scheduled non-diff runs sit `pending`: a local scheduled run parks for host dispatch, and "the scheduler does not auto-dispatch" (`docs/spec/workflow-v2.md:370`; #4526).
- Of 596 merged PRs, 83 were operator-authored outside any run (among them 30 ADR records, 11 instruction or skill, 9 workflow-config, 8 CI, 4 charter and 3 catch-rate re-pins; the report does not classify the rest), and 59 were Dependabot PRs merged by hand. These counts are the proposal's and were not re-derived by either review. Together the 83 and the 59 are 24% of merges outside the audit trail a run gives, part of it by decision: ADR records land by operator PR under ADR-113 D3.

**Constraints a catalogue must respect:**
- **Protection is a property of the complete diff, not a route.** ADR-121's seams "apply to the complete diff, and work on workflows with or without a plan stage" (`docs/adr/121-protected-judge.md:106`). At a tier-I plan gate the approval "IS the adoption" (`:116`). An agent proposes a tier-I change "never as an originated run" (`:138`). A workflow keyed by protected paths would not carry a change mixing ordinary and protected files.
- **Path predicates need a plan.** `applies_to.paths` is evaluated at the plan gate against the plan's `scope.files`, and declaring it with no plan stage is refused (`docs/spec/workflow-v2.md:312`). An escalation `match.paths` is likewise refused without a plan (`:451`).
- **`extends` cannot remove a stage.** Base stages keep their positions, a matching id merges and a new id appends (`docs/spec/workflow-v2.md:80`).
- **A pull request is an input, not a trigger.** Trigger sources are on-demand, scheduled, alert, issue, CLI and UI (`backend/internal/appliesto/appliesto.go:355-367`). A PR enters only as a stage input (`source: pull_request`, `docs/spec/workflow-v2.md:925`), as `human_led_change` declares it (`.fishhawk/workflows.yaml:1214`).
- **Settled exclusions:**
  - experiments are carried by existing workflows (ADR-136 D3, `docs/adr/136-planning-experiments-and-lifecycle-evidence.md:43`);
  - a retirement workflow type was deferred (ADR-134, `docs/adr/134-retirement-governance.md:27`);
  - the repository stays the unit of workflow policy (ADR-130 D1, `docs/adr/130-cross-repository-project-coordination.md:39`);
  - alerts already route to `hotfix_change` (ADR-053, `docs/adr/053-post-deploy-verification-incident-intake.md:23`).

  This record reopens none of them; it adds no family for any of them.

## Options

- **A. One workflow type per change shape.** This was the first proposal: separate protected, test, docs, dependency, eval-recalibration, investigation and security workflows. Both reviews rejected it. A protected-path lane cannot carry a mixed change. A docs variant can be declared as a standalone document today; omitting inherited stages through `extends` requires a new composition primitive. Every new type multiplies the six standalone preset documents.
- **B. A small set of families with presets, and cross-cutting controls (recommended).** Each family is distinct in where its diff comes from, what its effect lands on, and which gate authorizes it. Presets vary a family. Protection and confidential security handling bind across families, and test integrity is a direction. Missing primitives are built before new types.
- **C. Keep `feature_change` only and refine its presets.** This leaves adopted PRs and non-diff work without a product route, and leaves the hand-merged Dependabot PRs outside a run.

## Recommendation

**Option B.**

**D1. Five families.** A family is defined by three things: where its diff comes from (generated in the run, an existing pull request head, or none), what its effect lands on (the repository, the tracker, a decision record, a deployment target), and which gate authorizes that effect (merge, approval of proposed tracker mutations, ratification, deployment approval). The trigger and any required issue input vary inside a family. Stage count and order vary inside a family. A workflow whose effect passes more than one of these gates is classified by the effect the run exists to produce and the gate that authorizes it, and a workflow that fits no family needs an ADR before it enters the product library. The catalogue has five:
1. **Planned change.** Its diff is generated in the run; its effect lands on the repository; the merge gate authorizes it. Typical stages: Propose, a plan approval gate governed by the declared autonomy and applicable non-delegable controls, apply, independent review, acceptance where declared, then the merge gate. Today this is `feature_change`.
2. **PR adoption.** Bound to an existing pull request head; carries that pull request's diff and does not produce it; the merge gate authorizes it. A pull request someone other than the run's agent wrote enters bound to its head and is carried to the merge gate. Its stages, the order of verification and review, and whether the change is carried as written are decided by Direction (a). Until Direction (a) is ratified, the product library gains no family 2 preset. The authority of this repository's `human_led_change` remains governed by its existing declarations and ADR-121, including that record's decided instance fix for adoption and preserved single-maintainer voucher exception. No family 2 product build, and no family 2 preset with agent or delegated merge, starts before the trust-boundary ADR in Direction (a) is ratified; building what ADR-121 already decides, including its D5 adoption gate and its instance fix, is not such a build. This limits the product library and its builds; it adds no refusal for what a repository declares, which the existing grammar already admits (`docs/spec/workflow-v2.md:925`). Catalogue membership grants no execution or merge authority.
3. **Observe, propose, apply.** It produces no repository diff and has no merge; its effect lands on the tracker; its declared gates disposition reports and its declared action policy governs any tracker mutations. Report-only variants, including milestone scoping, may finish without applying mutations. Typical stages: collect evidence, propose, a human disposition, apply the approved mutations, confirm. Grooming, upkeep, user-report scanning and milestone scoping are this family.
4. **Governance decision.** It produces no repository diff of its own; its effect is a decision record, which lands under ADR-113 D3; ratification authorizes it. The `adr_change` workflow ADR-139 (#5007) decides; this record changes nothing in it.
5. **Release operation.** It produces no repository diff; its effect lands on a deployment target; the deployment approval authorizes it. Typical stages: qualify the candidate, authorize the deployment, delegate it, verify the outcome. This repository's `release` declares it without a candidate input; ADR-132 decides what a candidate is.

`routine_change` is a family 1 workflow without a plan stage, which D4 keeps out of the product presets; `upkeep_scan` and the user-report scan are family 3 workflows with no apply stage, whose approval gate itself files the approved findings.

Adding a family to the product library requires an ADR. The catalogue governs the product library: the preset documents `fishhawk init` writes (`docs/spec/workflow-preset.md`) and any opt-in document a later decision adds. The grammar examples under `docs/spec/examples/` and this repository's declarations illustrate the families and are not members of the library. It does not restrict what a repository declares, and no validator rule follows from it.

**D2. Presets vary a family; they do not fork it.** A preset sets a family's label envelope, path envelope, reviewer roster, budgets and autonomy. Until preset composition exists (D5), a preset is a complete document, as today. Once it exists, a new product preset omits a stage through it rather than by a new standalone copy. Existing documents are not made non-conforming by this record. Candidate presets:
- **Planned change:** hotfix (a shipped example today). Candidates, each decided when it is built: a small change (docs and chores; a lighter review roster; no acceptance stage) and a security fix. Whether a small-change preset may carry test changes waits on Direction (d).
- **PR adoption:** none until Direction (a). Its candidates are an operator-authored change (what `human_led_change` carries today), a dependency update and a contributor PR.
- **Observe, propose, apply:** grooming, upkeep, user-report scanning and milestone scoping (shipped examples today). No investigation preset is added: ADR-136 D3 has existing workflows carry that work.

**D3. Controls on every workflow, never lanes.** No workflow is selected or keyed by protected path:
- protected-change classification (ADR-121) applies to the complete diff of every change a run carries, whether the run produced it or adopted it, in whichever family, exactly as ADR-121 D3 and D4 decide; this record changes nothing in ADR-121;
- a test-integrity control, if one is decided, is decided by its own ADR (Direction (d)) and is never a tests-only workflow;
- confidential security-finding handling (ADR-126) applies in every family, as that record already decides for every `category=security` concern; this record changes nothing in it.

`human_led_change` remains the workflow for a change a human writes, as ADR-121 D4 states. This decides where the two decided controls bind, not how they are built.

**D4. Every product preset that generates a diff within its run retains a plan stage, for now.** This is a conservative library policy. The current grammar requires a plan for `applies_to.paths` and `escalations.match.paths` (`docs/spec/workflow-v2.md:312` and `:451`); it separately permits a post-hoc `constraints.allowed_paths` control (`:326`). This rule does not cover a diff that enters as an existing pull request, or an agent's change to one; Direction (a) decides both. Lifting this rule, including after a stage type that produces an authoritative pre-implementation path set exists (anticipated at `docs/spec/workflow-v2.md:328`), requires an ADR. A light plan with a smaller review roster is allowed. `routine_change` is not a product preset; this repository decides separately whether to retire it or rebuild it under this rule.

**D5. Primitives before types.** A gap that several families share is closed by a primitive, built under its own issue or ADR, before a new workflow type is added. The order of those builds is tracked on the epic, not decided here. Preset composition (D2), path-conditional checks in escalations, a stage type that produces an authoritative pre-implementation path set (D4) and further product-derived bound classes, if Direction (b) decides any, change the workflow grammar or an accepted record and each need an ADR; PR admission with head binding, beyond what ADR-121 D5 decides, waits for Direction (a). Implementation inside an already-decided contract may proceed under an issue; this record grants no exception for a new grammar or authority decision.

**D6. Product library versus this repository.**
- `fishhawk init`'s default stays the planned-change document.
- Whether and how each other family ships as an opt-in library document is decided when that family is built.
- Fishhawk-specific procedures stay this repository's configuration, not product types. Catch-rate recalibration remains this repository's procedure, followed by a protected record change. Admitting its credentialed evaluation work as a specialist job requires a separately designed extension to the closed specialist-job contract.

**Directions requiring their own ADR:** Each direction's ADR supplies its own end-to-end refusal and invalidation cases before its route is enabled.
- **(a) The PR adoption trust boundary, beyond what ADR-121 D5 decides:** trusted policy, head and base binding, bot identity, verification isolation for untrusted content, approval renewal after a rebase, author approval beyond ADR-121's instance fix and its preserved single-maintainer voucher exception, an agent's change to an adopted pull request (a fix-up, a rebase or a conflict resolution), and the test checks an adopted pull request receives.
- **(b) Surfaces beyond ADR-121's bound paths:** whether further control surfaces get a non-delegable merge under ADR-121 D4.2. Whether an agent may originate a tier-I change is decided (ADR-121 D5) and is not reopened here.
- **(c) Evidence and authorization across runs and external actions:** producer identity, freshness, partial failure, reconciliation and revocation.
- **(d) Test integrity:** whether a control refuses a weakened test, what evidence shows weakening, and how it applies to a diff that enters through PR adoption.

## Decision

**Accepted 2026-11-14** by the Captain (1 of 1; manual ratification path, the ADR-113 D2 verb is not yet built; approver, erratum and hashes in the ratification comment). **Option B**, i.e. D1–D6 as stated in the Recommendation, decided now, including D1's limit that no family 2 product build, and no family 2 preset with agent or delegated merge, starts before the Direction (a) ADR is ratified (building what ADR-121 already decides, including its D5 adoption gate and instance fix, is not such a build); Directions (a) to (d) are not decided and each requires its own ADR. The catalogue governs the product library, not what a repository declares. It amends no accepted record: D3 restates ADR-121 and ADR-126, D1 places ADR-139's workflow without changing it, and `human_led_change`'s authority stays ADR-121's. This record builds nothing by itself and changes no runtime permission.

## Consequences

- **Scope.** This builds nothing by itself. It changes no runtime permission and no schema.
- **Costs.** Each D5 primitive is its own build. Each opt-in library document a later decision adds is a standalone document until preset composition lands.
- **Residuals.**
  - D4 keeps a plan cost on small changes. The 11-04 test-lane experiment estimated, from one baseline run, that removing plan and plan review removes roughly 40–55% of its cost and the plan-approval wait; its token accounting was not comparable to a product run, and 29.9 h of that run's 34.8 h was human wait.
  - The run evidence is from this one repository.
  - A repository can already declare a pull-request input outside the library; nothing here refuses it, and Direction (a)'s rules do not exist yet.
  - The out-of-tree evidence holds only while that directory is kept.
- **Record metadata.** amends: []; supersedes: []; applies_to: []

### Validation

- **D1:** a table in the preset documentation assigns every product library document to exactly one family; a document that fits none is not added without an ADR.
- **D1.2:** a test over the product library asserts that no document declares a `source: pull_request` input; the Direction (a) ADR changes or removes it.
- **D3:** no product library document declares `applies_to.paths` or an escalation `match.paths` whose stated purpose is to route a protected surface to a different workflow; the ADR that adds a document records this.
- **D4:** a test over the product library checks each resolved workflow covered by D4 and asserts that a plan stage precedes every stage producing the `pull_request` artifact.
- **D6:** an onboarding test asserts that `fishhawk init`, when no other family is selected, writes for every tier and shape a document whose workflows are all family 1.
