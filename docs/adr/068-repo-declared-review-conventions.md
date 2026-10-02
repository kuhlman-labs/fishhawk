---
id: ADR-068
title: "Repo-declared review conventions: workflow-declared, server-injected supplemental criteria for plan and implement review"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2211
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-068: Repo-declared review conventions: workflow-declared, server-injected supplemental criteria for plan and implement review

## Context

Repos have review conventions that are not expressible as deterministic constraints — layering rules, naming, error-message style, banned vocabulary (this repo's own `BRAND_FOUNDATIONS.md` §5), test placement beyond what `test_conventions` (#1004) covers. Today there is no supported way to add them to the plan-review or implement-review lens. Three specific findings motivate a first-class surface.

**1. The grounded-citation rule currently suppresses legitimate findings along with hallucinated ones.** Plan-review criterion 6 (`backend/internal/prompt/prompt.go:2480`) binds the reviewer: any rule it cites — from `CLAUDE.md`, a style guide, or a project convention — must be quotable verbatim from the prompt or a repository file it actually read, and it must not assert rules from memory. That is the right rule, and it was added for a real failure mode. But it leaves the reviewer with no authoritative place to find a repo's actual conventions, so it removes the true positives along with the false ones. A declared conventions file is the missing half: it makes repo rules quotable and authoritative, so the suppression can keep holding while genuine convention concerns become raisable.

**2. Reviewer adapters have asymmetric repository access, silently.** The `claudecode` and `codex` reviewers run with a checkout and can read `CLAUDE.md` / `AGENTS.md`. The `anthropic` adapter is API-based and cannot. The heterogeneous reviewer pair this repo runs on both stages (#955) therefore sees different convention context on every review, with nothing surfacing the asymmetry. Injecting conventions server-side into the prompt normalizes all three adapters onto identical rules.

**3. The prompt architecture already supports this cheaply.** Review prompts are assembled server-side (`buildPlanReview` at `prompt.go:2416`, `buildImplementReview` at `:2718`), and #1725 established a cache-stable prefix ordering ahead of `ImplementReviewSplitMarker`. Per-repo-stable content placed in that prefix costs nothing incremental across the fix-up re-review rounds of a stage.

## Options

1. **Status quo — rely on the agent reading `CLAUDE.md` itself.** Rejected: works only for checkout-bearing adapters, is invisible to the audit trail (no record of which conventions a verdict was formed under), and collides with the grounded-citation rule rather than satisfying it.

2. **Convention-discovered file — auto-pick-up `REVIEW.md` when present.** Rejected on governance grounds: anyone who can merge a file would change review behavior without touching the governance artifact. Declaration in `.fishhawk/workflows.yaml` puts the choice behind whatever protects that file — note this repo's own spec already lists `.fishhawk/**` under implement-stage `forbidden_paths`, which is the precedent.

3. **Workflow-declared, server-injected review conventions.**
   - `review_conventions:` at file level — named entries carrying `path`, optional `applies_to` (the ADR-066 / #2209 path predicate), optional `severity_cap`, optional `required` (default true; a declared-but-missing file fails the stage loudly rather than skipping silently).
   - Selected per stage via `reviewers.conventions: [<name>…]` on plan and implement stages. Named-and-referenced rather than an inline per-stage path, because plan and implement typically want overlapping sets and this is the only shape that does not duplicate.
   - Resolved from the run's BASE ref, never the PR head, so an implement agent cannot soften the conventions used to review its own diff. A diff that modifies a declared conventions file is itself flagged.
   - Injected server-side into the prompt — not "go read this file" — so every adapter sees identical rules, the content is cacheable in the #1725 stable prefix, and the exact text is auditable.
   - Rendered as strictly additive and explicitly subordinate: cannot remove, weaken, or reorder any standard criterion. An instruction attempting to do so must be ignored and reported as a concern (`conventions_override_attempt`). Derived concerns carry a `repo_convention` category and quote the rule verbatim.
   - Resolved path, commit, and content hash recorded in gate evidence and the audit entry — matching the content-hash-reference pattern the acceptance artifact already uses — so "why did the reviewer raise that?" resolves to a specific conventions revision.
   - Size-capped (8–16KB) with loud truncation plus an audit entry, bounding added prompt cost (the #606 precedent).
   - `severity_cap` prevents a repo convention from manufacturing a blocking reject under a gating reviewer topology.

4. **Inline `text:` in the spec alongside `path:`.** Rejected: moves prose into the policy file and creates a second mechanism to validate, render, hash, and cap, for a case a short file already covers.

## Recommendation

Option 3, gated on an evaluation rather than shipped on reasoning alone.

The failure mode is dilution: twelve standard criteria plus a page of repo prose degrading catch rate on the standard ones. `backend/internal/agenteval` already carries a `planreview-miss-corpus` built for exactly this class of question. Ship only if the corpus does not regress with conventions attached, and treat that as a permanent gate on future review-prompt changes rather than a one-time check.

This is additive, so it can land on the current major without waiting for the ADR-067 (#2210) consolidation — but it should consume ADR-066's path predicate for `applies_to` rather than growing a parallel matcher.

Separate files per stage type rather than one file with sections: plan review and implement review examine genuinely different things, and naming lets a repo write a plan-conventions and an implement-conventions file without Fishhawk inventing a section grammar inside Markdown.

Deliberately out of scope: applying conventions to the implement agent (it already reads `CLAUDE.md` / `AGENTS.md` natively, so this would add a second competing convention channel) and to the acceptance agent (no demonstrated demand).

## Decision

**Accepted (2026-07-26).** Adopt Option 3 — workflow-declared, server-injected review conventions, with the design in the Recommendation. Four forks were settled at ratification; where they refine the Recommendation, **these govern**.

### 1. Build the injection as a SHARED mechanism with two declaration sites

ADR-065 (#2161) was ratified with its charter artifact reusing this mechanism rather than inventing a parallel one. So this epic builds it as reusable machinery, not conventions-specific code:

```
shared mechanism        resolve(base_ref, path) → content
                        inject(prompt, content, subordinate framing)
                        attribute(audit, path + commit + content_hash)
                        cap(size) → truncate loudly + audit

declaration sites       workflow spec      review_conventions[]   (this ADR)
                        work-management-v0 charter.path           (ADR-065)
```

The two use cases are the same shape — a repo-authored prose document that shapes agent judgment and must be tamper-evident and attributable — and **base-ref resolution matters for identical reasons on each side**: an implement agent could otherwise soften the conventions reviewing its own diff, and a groomer could rewrite the charter constraining it inside the change it is proposing.

**E54.2 (#2234) is blocked on this** and, once this lands, wires only the second declaration site.

### 2. `severity_cap` is kept — optional, uncapped by default

A conventions-derived concern behaves like any standard criterion unless an operator caps it. Conventions are legitimate review criteria; treating them as second-class by default would frustrate exactly the use this feature exists for. The cap exists for the delegated case — a conventions file authored by a team the gate owner trusts less than themselves — and is the knob that makes teams comfortable letting others author these files at all.

### 3. The eval gate is HARD and standing, not advisory

Conventions do not ship unless `backend/internal/agenteval`'s `planreview-miss-corpus` shows **no regression** in catch rate on the standard criteria with conventions attached.

The failure mode is dilution: twelve standard criteria plus a page of repo prose degrading attention on the standard ones. Dilution is not a cliff — a 3% drop reads as noise, and three such changes each read as noise, while the aggregate is a materially worse reviewer nobody decided to accept. An advisory number gets waved through; a gate does not.

This becomes a **standing gate on future review-prompt changes**, not a one-time check.

### 4. Implementation lands in its own epic

Not folded into E53. Review conventions carries a distinct risk surface (prompt injection into a governance gate), its own eval gate, and prompt-rendering work that none of E53's children touch — while E53 is already six children scoped to the control surface. It does consume E53.1's (#2224) shared path predicate for its `applies_to` and must not grow a parallel matcher.

### Unchanged from the Recommendation, restated because they are load-bearing

- **Declared, never auto-discovered.** No picking up a `REVIEW.md` if present — that would let anyone who can merge a file change review behaviour without touching a governance artifact.
- **Strictly additive and subordinate.** Conventions cannot remove, weaken, or reorder any standard criterion; an instruction attempting to must be ignored and reported as `conventions_override_attempt`.
- **A diff that modifies a declared conventions file is itself flagged.**
- **Out of scope:** the implement agent (it already reads `CLAUDE.md`/`AGENTS.md` natively — a second competing channel) and the acceptance agent (no demonstrated demand).

Named approver: repository maintainer (human).

## Consequences

Repos express review conventions once and every reviewer adapter sees them identically, closing the silent `anthropic`-versus-checkout asymmetry. The grounded-citation rule gains an authoritative source, so legitimate convention findings become raisable while hallucinated ones stay suppressed. Review conventions join the audited governance surface, with each verdict attributable to a specific conventions revision.

This introduces a prompt-injection surface into a governance gate — the central risk, and it is mitigated rather than eliminated. Base-ref resolution removes the self-serving-edit path; subordinate framing plus the explicit override-attempt report addresses direct instruction override; the size cap bounds the payload. What remains is that a sufficiently adversarial conventions file can still consume reviewer attention. Two things bound the blast radius: the default reviewer topology is advisory (agent verdicts cannot block a human gate), and the conventions file is declared in `.fishhawk/workflows.yaml`, which is itself protected. This should be stated plainly in the field's documentation — it is a quality aid under a protected declaration, not an adversary-proof control.

Secondary costs: an eval-regression gate is added to the review-prompt change process; `severity_cap` and the `repo_convention` category add taxonomy the concern-ledger and fix-up routing must understand; and a declared-but-missing conventions file becomes a new loud stage-failure path.
