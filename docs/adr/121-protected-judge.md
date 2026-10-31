---
id: ADR-121
title: "The protected judge: agents cannot change what judges their work without a human"
status: accepted
date: 2026-10-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/4727
supersedes: []
superseded_by: []
amends: ["ADR-101", "ADR-112", "ADR-118"]
applies_to: []
---

# ADR-121: The protected judge: agents cannot change what judges their work without a human

## Status

**ACCEPTED (v4.1, ratified by the Captain 2026-10-26).** Drafted by the first officer on 2026-10-26 at the Captain's direction.
- **Review history:**
  - v1 (combined) was rejected and split.
  - v2 was rejected by both reviewers.
  - v3: claude-fable-5-1 approve_with_concerns; gpt-6-astra (Codex) reject.
  - v4 folds in every v3 finding.
- **Companion:** ADR-122 (the self-improvement charter) depends on this record.

The Captain ratifies.

## Context

Agents change Fishhawk through Fishhawk: implement agents write code, operator agents approve plans under the standing order, and the queue merges. What judges that work:
- **Tests and evals:** eval harnesses and baselines (ADR-068 D3, ADR-118), and the verify gate.
- **Reviewers:** review and persona prompts, remits, conventions, reviewer configuration, and the decision record the architect reads.
- **Authorization:** the forge-approval predicate (ADR-112), the dispatch ledger (ADR-109), ADR ratification (ADR-113), the audit chain, credentials and scopes.

**Protected today:**
- `agent_instruction_paths` (#3942) and forbidden paths;
- `check-review-prompt-eval`, which is hand-run;
- ADR-101 D2's charter-mode diff refusal.

**Unprotected:** an implement agent may edit remits and conventions, eval corpora and baselines, the reviewer configuration, `docs/adr/**`, and compliance evidence. Each changes what judges the next change.

**The volume involved.** Since 2026-10-01, agent runs merged roughly 130 PRs touching judge MACHINERY (51 under `backend/internal/audit`, 36 under `scripts/`, 25 under `agenteval`, 17 under `docs/compliance`), and almost none were human-made. Human origination for all of it would stop product work, so inputs and machinery need different treatment.

## Options

- **A. Status quo plus convention.** Rejected: the holes are unenforced.
- **B. One tier: everything is human-originated.** Rejected: it would halt product work.
- **C. Two tiers: judge INPUTS are human-adopted; judge MACHINERY stays agent-buildable behind seams and a non-agent merge verdict (recommended).**

## Recommendation

**Option C.**

**D1. Two tiers, defined by function, not by location.**
- **Tier I, judge inputs.** Anything that decides WHICH judgements run, WHAT a judge is told, or WHERE a judge's thresholds, bindings or class lists lie, wherever it lives:
  - reviewer remits and conventions;
  - persona definitions;
  - eval corpora, labels, fixtures, baselines and evidence records;
  - the eval trigger list;
  - the verify harness list (the set of checks the gate runs);
  - the reviewer configuration in the workflow spec;
  - the decision record and its index;
  - the charters;
  - agent instruction surfaces (#3942);
  - every constant table or threshold that defines a binding, a class list or a catch rule, wherever it lives. Examples: `agentpaths`' class table, `testdata/policy/*.json`, and the catch-rate gate's tolerance and power floor;
  - this record's `protected_paths` declarations.

  A file that mixes tier-I content with code is tier I, until the tier-I content is extracted into a separately declared file.
- **Tier M, judge machinery.** Code that EXECUTES judgements, with no tier-I content in it:
  - eval harness execution code;
  - the verify runner;
  - audit-chain and evidence writers;
  - the forge-approval predicate's execution;
  - the dispatch ledger;
  - credential and scope handling;
  - the classifier and constraint execution code.

  Each is named by its specific files, not by directories.

**D2. Bindings are declarations (ADR-110).**
- **Derived bindings.** The product derives bindings from existing declarations: `review_conventions`, persona remit paths, the decision-record location, the compliance directory, `agentpaths`, and the WHOLE workflow spec file. `.fishhawk/**` is already operator-authored.
- **Declared bindings.** Every other path is declared in a `protected_paths` block (tier I or M) in the workflow spec. Fishhawk declares its own paths there. There is no layout default.
- **Changing bindings.** A repository may add paths, and may declare a product class `absent` (for example, no personas). An `absent` class counts as bound. A repository may never remove a product class. Changing a binding, an `absent` declaration, or a removal is a tier-I change.
- **Unbound classes.** A class that is neither derivable, declared nor `absent` is `unbound`.
  - `fishhawk_doctor` reports it.
  - ALL unattended execution in that repository is refused, whether charter-driven (ADR-101) or ordinarily delegated (campaigns, high-autonomy workflows).
  - Attended work continues only under an explicit non-agent authorization that records the missing protection. Session presence alone does not qualify.

**D3. Classification by effect, against trusted policy.**
- **What makes a change protected.** A change is protected (tier I or M, the stricter winning) when its COMPLETE resulting diff edits a bound path, or a declared dependency of one.
- **Which policy applies.** The policy at the change's base commit, never the policy the change introduces.
- **When it is re-checked:**
  - after every amendment;
  - after a fix-up;
  - after a slice fan-in;
  - after a conflict resolution;
  - after a base advance.

  Reverts and descendants are classified the same way.
- **Durable dependencies.** ANY protected change, in either tier, that introduces or redirects a judge's read of a path must declare that path in `protected_paths` in the same change. That declaration is tier I, so it needs adoption (D5).
  - The post-implement gate HOLDS a change to a bound file whose diff introduces a new path literal, file read or import that is not declared.
  - Only a tier-I adoption of the declaration clears the hold.
  - Existing dependencies are declared before an enablement that relies on them.
  - Whole-graph analysis stays ADVISORY: a digest entry and a review finding.
- **Unknown classification.** A change whose classification cannot be computed is HELD.

**D4. Seams.** These are built in, apply to the complete diff, and work on workflows with or without a plan stage.
1. **The scope constraint.** `protected_judge_paths` is checked at the plan-gate scope precheck, at the post-implement gate, and at merge. It cannot be waived by a stage (no `enforce: false`). Existing forbidden-path constraints still bind independently.

   | Change | Verdict |
   |---|---|
   | tier I with no implementation authorization (plan approval, or the plan-less protected-scope authorization in D5) | refuse |
   | tier I, authorized, but the resulting head is not yet adopted (plan-less workflows) | resumable HOLD at the built-in adoption gate (D5), never a failure |
   | tier I with a valid adoption | allow; the tier-I escalation applies |
   | tier M | allow; the tier-M escalation applies |

   **At the plan gate, the approval being evaluated IS the adoption.** The gate accepts a tier-I plan only on a non-agent approval, recorded as the adoption. A delegated or standing-order approval is refused there.
   | unknown | hold |
2. **Merge.** A merge whose PR diff touches a bound path requires a forge-verified, non-agent approver (ADR-112 R1, `not: [author, agent]`). Per ADR-113, this does not prove a human acted. ADR-112's single-maintainer voucher exception is preserved. A delegated or charter-driven identity never satisfies it. This extends ADR-101 D2.
3. **Escalation.** A built-in escalation evaluates D3's classification of the COMPLETE diff at EVERY delegated action (approve, fix-up, retry, waive, merge), not plan scope at the approval gate. It clamps the run's own workflow to:
   - `max_autonomy: low` for tier I;
   - `max_autonomy: medium` for tier M.

   It composes with any stricter existing ceiling (the minimum wins). It is valid on workflows that are already `low`, and on workflows with no plan stage. `human_led_change` remains the workflow for a change a human writes.

**D5. Tier-I adoption is the Captain's act.**
- **What adoption is.** A tier-I change requires a recorded ADOPTION, `protected_change_adopted`.
- **On a workflow WITH a plan stage,** adoption is the non-agent plan approval, bound to the plan revision and its scope. It both authorizes implementation and adopts the change.
- **On a workflow WITHOUT a plan stage,** permission to implement and adoption of the result are separate steps:
  1. **Before implementation,** the Captain authorizes the run's protected SCOPE.
  2. **After implementation,** the candidate PR is published for review, and a missing head-bound adoption is a resumable HOLD, not a constraint failure.
  3. **The adoption gate.** A built-in, non-delegable adoption gate is exposed even when the workflow declares only a `check` gate (for example `routine_change`).
  4. **Adoption.** Its approval records `protected_change_adopted` against the exact PR head, and permits the run to progress.
  5. **Renewal.** Every new head needs renewed adoption. A scope expansion also needs renewed implementation authorization.
- **No delegation.** A standing plan-approval order NEVER covers tier-I adoption. Like ratification (ADR-113 D2), it is the Captain's act.
- **What adoption permits.** Implementation authorization permits agent implementation, and adoption permits progression to merge. Neither disables classification, a seam, the merge verdict, or the independently enforced forbidden paths.
- **Invalidation.** A material amendment (scope or criteria) voids both. A new PR head on a plan-less workflow voids the adoption.
- **Pre-registration.** Where ADR-118 applies, the change carries a pre-registration authored or approved by a non-agent. This tightens ADR-118 D5 to tier I.
- **How the loop proposes tier-I changes.** An agent proposes a tier-I change only as a crew finding or a filed proposal, never as an originated run.

**D6. Honest residuals.**
- A process holding a human's credentials can act as that human (ADR-113).
- A human can approve without reading. Visibility and ADR-101 D8's sampling audit are the only controls: every protected merge is in the digest with its authority delta.
- Reads that are not visible as path literals or imports (computed paths, network fetches) escape the D3 hold. The advisory graph analysis and the adversarial tests report them.

## Decision

**Accepted 2026-10-26** by the Captain (repository maintainer): **Option C**, i.e. D1–D6 as stated in the Recommendation (v4.1), decided now. It amends ADR-101 (D2's diff refusal is extended), ADR-112 (the merge verdict gains the protected-path condition; the single-maintainer exception is preserved) and ADR-118 (D4 reviewer configuration becomes tier I; D5 authorship is tightened). Implementation lands after the 2026-11-01 re-pin, with this repository's `protected_paths` declaration in step 1.

## Consequences

- **Gains:**
  - the judge's inputs cannot change without the Captain;
  - its machinery stays agent-buildable at today's pace, behind a non-agent merge verdict;
  - the protection is portable by declaration.
- **Costs:**
  - `protected_paths` with tiers and `absent`;
  - binding derivation and `fishhawk_doctor`;
  - the constraint and its verdict table;
  - the diff-evaluated escalation;
  - the merge condition;
  - `protected_change_adopted`, with no delegation;
  - the effect classifier, its re-checks and the dependency hold;
  - extracting tier-I constants out of code files;
  - the METHODOLOGY authorship paragraph update.
- **Adversarial acceptance tests** (scheduled; ADR-122 treats them as "control exercised"). Each must be refused or held:
  - a judge change split across two PRs;
  - a dependency introduced by a tier-M change;
  - an edit smuggled through a fix-up, amendment or fan-in;
  - a revert;
  - a binding change or removal;
  - a class declared `absent` without adoption;
  - a delegated identity merging;
  - the standing order attempting adoption;
  - an unknown classification.
- **Implementation order:**
  1. The D2 bindings and doctor, with tier-I constant extraction.
  2. The D4 constraint and escalation.
  3. The D4 merge condition.
  4. The D3 re-checks and the dependency hold.
  5. The D5 adoption.
  6. The tests.

  It lands after the 2026-11-01 re-pin.
- **Rollout notes:**
  - **Bindings first.** The repository's `protected_paths` declaration lands WITH step 1. Otherwise every declared-only class reads `unbound` and stops unattended work.
  - **`scripts/test` is mixed.** It inlines the verify harness list, so it is a mixed (tier-I) file until the list is extracted, and Captain adoption of `scripts/test` edits is an acknowledged cost until then.
  - **Instance fix for adoption.** In this repository, plan-less adoption at `human_led_change`'s review gate needs that gate to declare `not: [agent]` rather than `not: [author, agent]`, as ADR-113 did for ratification, so that the Captain can adopt a change he vouched.
- **Record metadata.** amends: ["ADR-101", "ADR-112", "ADR-118"]; supersedes: []; applies_to: []
- **Related:** ADR-068, ADR-083, ADR-109, ADR-110, ADR-113, #3942, `docs/METHODOLOGY.md`.
