---
id: ADR-142
title: "Keep merge verification requirements across acceptance scenario commits"
status: accepted
date: 2026-11-06
issue: https://github.com/kuhlman-labs/fishhawk/issues/5088
supersedes: []
superseded_by: []
amends: ["ADR-090"]
applies_to: []
---

# ADR-142: Keep merge verification requirements across acceptance scenario commits

**Status: accepted (ratified 2026-11-06).**

## Context

ADR-090 D2 requires a passing merge-candidate verify at the exact head produced by a base advance, a conflict-resolution push or a decomposed-parent integration. Runner-gated implement and fix-up heads need no additional pass. Acceptance can subsequently append a scenario-corpus commit C. The current classifier has no scenario-commit arm, so a standalone scenario head can fall through to not_required even after an unverified base advance or conflict push.

#4874 also exposes a separate choice: a scenario report can be missing, malformed, cyclic or unrelated enough that its reported predecessor cannot be tied to a recorded write or a runner-gated root. ADR-090 Amendment 1 specifies an undecidable-fails-closed rule for operator-vouched heads; it does not explicitly decide this scenario-chain case. This amendment decides that case directly rather than extending the vouch rule by analogy. ADR-090 Amendment 1's verified reference can also name a passed scenario commit. Applied to a vouched terminal, that reference would let evidence at an intermediate commit, or at the current head itself, erase the verification cause, so this amendment decides that use too.

The amendment is about merge-candidate classification. It does not decide whether an operator-vouched commit advances acceptance's expected head, the separate #4856 question.

## Options

- A. Preserve the current scenario-head fallback. It avoids an additional pass for unresolved reports but can treat an unknown chain as having no verification requirement.
- B. Preserve recorded requirements across scenario commits, and require an exact-head pass for an unresolved scenario chain on a forge-probeable run. This costs verification and operator recovery time when the chain cannot prove the exemption.
- C. Require verification for every live head. This would broaden the rule to runner-gated, park and held-commit flows that ADR-090 D2 deliberately leaves outside it.

## Recommendation

Adopt Option B, with the following narrow rules.

D1. A scenario-corpus commit does not discharge an outstanding requirement created by a recorded ADR-090 D2 producer. When the newest applicable non-no-op recorded head write preceding C is a performed base advance, a conflict-resolution push or a fan-in integration, C requires passing evidence at exactly C. A report's base field cannot clear that recorded requirement. A branch_rebased row whose already_up_to_date field decodes as true is a no-op, so it is excluded before selecting that newest write. A malformed or missing no-op flag does not prove that no write occurred. A branch reset alone does not discharge the recorded requirement.

D2. A scenario chain whose reported terminal is the newest runner-gated reported head, with no later recorded D2 requirement, keeps its existing classification: not_required on a standalone run, and the existing fan_in answer on a decomposed parent. This is not a universal every-head rule. Existing classification of an operator-vouched live head, including ADR-090 Amendment 1's probe, determinability ladder and verified reference, takes precedence unchanged. A scenario chain whose reported terminal is an operator-vouched head, where D1 does not already decide the answer, uses Amendment 1's probe and determinability ladder on that terminal to determine a verification cause, with one change to its reference. For this use, the reference is the newest head by chain sequence among those Amendment 1 considers, excluding the terminal and every head that a scenario-corpus row on the run's chain reports. The exclusion is by reported head, so it covers the current head and any intermediate or forked scenario commit, whatever its result, its transfer row or its position relative to the vouch. It is not a temporal bound: a newer runner-gated head, and a passed or enforce-mode transferred head that no scenario-corpus row reports, stay eligible. An empty cause leaves a standalone run not_required and preserves a decomposed parent's existing fan_in fallback; it does not grant the direct live-head vouch exemption to a scenario commit. This amendment grants no new vouch authority and changes no acceptance-head resolution rule.

D3. When no recorded requirement already decides the answer, and a scenario chain is not resolved by D1 or D2, a forge-probeable run conservatively requires a passing verify at exactly C. Missing, malformed, empty, self-referencing, cyclic or unrelated predecessor metadata cannot establish the exemption. The existing base_advance cause represents this conservative verification requirement; it is not evidence that a base merge actually occurred. If the deployment cannot perform the existing forge probe at all, it retains ADR-090 D7's existing inability-to-probe residual with a warning, but only when no requirement is already recorded. In classification, inability to probe never clears D1's recorded requirement; ADR-090 D7's merge gate, which proceeds with a warning before classifying on a deployment that cannot probe, is unchanged.

D4. Audit or required forge read errors remain errors through the existing fail-closed paths. They are not a clean absence of evidence, an exempt chain, or the inability-to-probe residual. Workflows without a declared verify command retain ADR-090 D4's not_required answer. Initial PR, fix-up and non-scenario classification outside these rules remains unchanged.

D5. Verification results remain exact-head results. A passed, failed or in-flight result at a predecessor or an intermediate scenario commit neither clears nor fails C. A result at any scenario commit, including C, also cannot remove D2's terminal-vouch cause; once that cause exists, only a result at exactly C decides C. D2's reference answers a different question, whether the terminal brought in a base commit the reference lacks, so a verified head outside the scenario set can yield an empty cause without being a result at C. No verdict is inherited and no transfer row is fabricated. Existing ADR-096 transfer modes and eligibility remain unchanged. Recovery uses the existing rebase and verify-only path at C, subject to its existing queue and eligibility guards; an already-up-to-date row cannot mask the recorded requirement. A failed pass at C keeps the existing failed-result recovery. No new recovery authority or bypass is introduced.

## Decision

**Accepted 2026-11-06** by the Captain (manual ratification path; approver and hashes in the ratification comment). Adopt Option B and D1-D5. This amends the surviving scenario-head classification in ADR-090 D2, narrows ADR-090 Amendment 1's reference for D2's scenario-terminal use only, and states how its surviving D4 and D7 boundaries apply. ADR-092, ADR-093, ADR-096, ADR-098 and ADR-115 govern their existing queue, landing and transfer subjects and are unchanged; none supplies a contrary scenario-commit classification. All other decisions remain as written.

## Consequences

A scenario commit after a recorded D2 producer can cost another full pass, even when its predecessor passed: the evidence must name the current head. An unresolved scenario chain on a probeable run also costs a pass or operator recovery instead of silently taking an exemption. A reset or an unreadable legacy report can therefore cause conservative extra work. Queue admission and review prerequisites can delay recovery; this amendment promises no immediate dispatch. Scenario evidence cannot retire a terminal-vouch cause, so each scenario commit on a base-bringing vouched terminal costs its own pass, even when an earlier scenario commit passed. An operator vouch on a verified scenario commit can cost one too, because that scenario commit cannot serve as the reference. A directly vouched live head keeps its existing reference and can read not_required where a scenario commit on the same chain reads unverified; that difference is deliberate, and this amendment does not harden direct vouches. Classifying such a chain costs the same pull-request read and compares as classifying a vouched head, on every reader.

The inability-to-probe residual remains an explicit limitation. A verified head outside the scenario set that descends from the terminal yields an empty compare and so an empty cause, as it does for a direct vouch; D2 does not detect that shape. A readable audit chain is not proof that a reporting runner was honest, and this amendment adds no forge-ingestion, path-set or actor-authorship guarantee. The publisher's head-targeting residual, acceptance dispatch/prompt snapshot gap and recovery-child binding remain the separate work tracked by #5087, #5082 and #5083.

No new audit category, state, wire field, permission, transfer mechanism or universal verification policy is decided. The change must preserve existing vouch, runner-gated-root and no-command behavior and the exact-head merge gate.

### Validation

Use discriminating scenario chains after each recorded D2 producer, with probeable and unprobeable fixtures. Include no-op rows after performed writes, malformed no-op flags, resets, unrelated newer writes and genuine runner-gated-root controls. For unresolved chains, distinguish a healthy read containing insufficient metadata from an actual read failure and from an unavailable probe configuration. For a vouched terminal, combine absent, passed, failed and in-flight results at C with passed intermediate scenario commits in any number, a forked scenario commit and a vouch recorded after the scenario row. Keep a newer runner-gated reference, a verified reference outside the scenario set, an empty cause on both run shapes, recorded-requirement priority and direct live-head vouch controls.

Verify opposite evidence at C, its predecessor and an intermediate scenario commit: only evidence at C decides C. Preserve existing transfer-mode and vouch controls. Drive the real merge refusal and existing rebase/verify-only recovery, naming any queue hold truthfully. Independent mutations must reach named behavior assertions; compilation errors, skipped tests and timeouts are not detection.

**Record metadata.** amends: ["ADR-090"]; supersedes: []; applies_to: []

**Related:** ADR-035, ADR-092, ADR-093, ADR-096, ADR-098, ADR-112, ADR-124, ADR-141; #4874, #4856.
