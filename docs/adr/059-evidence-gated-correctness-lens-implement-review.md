---
id: ADR-059
title: "Evidence-gated correctness lens in implement-review"
status: accepted
date: 2026-07-12
issue: https://github.com/kuhlman-labs/fishhawk/issues/1883
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-059: Evidence-gated correctness lens in implement-review

## Context

The implement-review reviewer prompt (`buildImplementReview`, `backend/internal/prompt/prompt.go`) is **product code shipped to every repo Fishhawk drives**. Its default (no-gate-evidence) branch tells reviewers correctness is already handled and instructs them NOT to hunt for correctness bugs:

- `prompt.go:2637-2639` — "Mechanical correctness is already gated upstream: the policy gate, the test suite the implement agent ran, build/lint, and CI all check that the change is present and well-formed."
- `prompt.go:2641-2644` — "Do NOT generic-bug-hunt. Hunting for arbitrary bugs overlaps the test suite and CI and is the lowest-orthogonality lens; spend the review on the three lenses below instead."

The reviewer is steered to three orthogonal lenses instead: security/authz, test vacuity, and untested error/edge/concurrency paths (`prompt.go:2645-2660`).

**Two problems with the "already gated upstream" premise:**

**1. It is unverifiable for an arbitrary repo.** Fishhawk asserts this on behalf of a customer repo whose gates it neither controls nor inspects. That repo may have no coverage gate, no integration tests, or a CI that only lints. The product cannot substantiate "correctness is gated upstream" generically.

**2. The no-evidence default is inverted.** The `GateEvidence != nil` prompt variant (`prompt.go:2625-2635`) already exists — it was added because the unconditional claim once "licensed the run-07bce059 reviewer to produce a careful text-level verdict on a head the gates knew did not compile" (`docs/ARCHITECTURE.md:235`). But the DEFAULT branch, taken when the product holds NO concrete gate evidence, still asserts "already gated" and still says "do NOT generic-bug-hunt." Absent evidence is precisely when the correctness lens should be ON — and it is the branch that suppresses it hardest.

**Evidence the premise fails even in this repo.** For a correctness bug on a code path with no test or a vacuous test, none of the four cited gates catch it:

| Gate | What it verifies | Catches untested/vacuous-test bug? |
|---|---|---|
| Policy gate (`backend/internal/policy/policy.go`) | forbidden/allowed globs, max-files, `required_outcomes`; `tests_added_or_updated` is satisfied by a test-*named* file existing (`isTestPath`) — never reads the test body; also vacuously satisfied for docs/scripts/config-only diffs | No — presence/scope only |
| Verify gate (`scripts/test verify`, runner `main.go:1590`) | lint + schema-sync + `go test -race`; blocking (non-zero exit → `res.OK=false`) | No — only exercises existing tests |
| Coverage gate (`scripts/check-coverage.py`) | whole-repo AGGREGATE ≥80%, no diff awareness; CI-only, omitted from verify | No — new code can be 0% covered and still pass |
| CI (`ci.yml` / CI Pass) | re-runs the same suite + lint/build + aggregate coverage; rollup only | No — no fuzz/property/integration beyond the same unit tests |

The review's own lens-2 text ("CI passes a vacuous test; only a reviewer reading the test body catches it") concedes the test gate does not guarantee correctness.

## Options

**A. Status quo.** Keep trusting upstream gates unconditionally in the default branch. Rejected — demonstrably licenses text-level verdicts on unverified heads (`ARCHITECTURE.md:235`) and is unverifiable across customer repos.

**B. Always enable a general correctness bug-hunt lens.** Rejected — an LLM bug-hunt is lower-signal than a deterministic gate and dilutes the orthogonal lenses; contradicts the sound orthogonality rationale.

**C. Evidence-gate the correctness-deprioritization (recommended).** Make suppression of the correctness lens conditional on the product actually holding gate evidence:
1. **Invert the default.** When no concrete gate evidence exists for a run, the prompt ENABLES a correctness lens; only suppress bug-hunting on the `GateEvidence != nil` path where passing gate evidence is present.
2. **Make `required_outcomes` verification-substance-aware, not shape-aware.** Augment/replace the test-file-name check with an outcome requiring the implement agent to report what it ran and what passed, gating on that evidence — generalizes across repos where Fishhawk cannot run the customer's coverage tool.
3. **Offer diff-coverage as a workflow-spec constraint** (customer's configured coverage command, scoped to the PR diff) so new lines must be covered where a customer opts in.

**D. Repo-local coverage fix only.** Add patch-scoped coverage to `scripts/check-coverage.py` + run it in-loop in `scripts/test verify`. Necessary but insufficient — fixes only Fishhawk's own dogfood gates, not the product surface shipped to customers. Track as a secondary follow-up, not the ADR decision.

## Recommendation

Option **C**. Evidence-gate the correctness lens: invert the no-evidence default, make `required_outcomes` substance-aware, and expose diff-coverage as a spec constraint. Keep vacuous-test detection as an explicit reviewer responsibility (lens 2) — it is not deterministically gateable; optional future backstop is periodic mutation testing over changed packages (out of scope here). Repo-local patch-coverage (Option D) is a secondary follow-up.

## Decision

**Accepted — Option C** (2026-07-12). Evidence-gate the correctness-deprioritization:
1. **Invert the no-evidence default** in `buildImplementReview` so the reviewer prompt ENABLES a correctness lens when the product holds no concrete gate evidence; suppression of bug-hunting stays only on the `GateEvidence != nil` path.
2. **Make `required_outcomes` verification-substance-aware** — the implement agent reports what it ran and what passed, gated on that evidence, rather than the shape-only test-file-name check.
3. **Expose diff-coverage as an opt-in workflow-spec constraint** (customer's coverage command, scoped to the PR diff).

Vacuous-test detection remains a reviewer responsibility (lens 2); periodic mutation testing is an optional deferred backstop, out of scope. Repo-local patch-coverage (Option D) is a secondary follow-up. Implementation tracked in **E46** (#1884); primary fix filed as **E46.1**.

## Consequences

- Reviewers spend budget on a correctness lens whenever the product cannot prove upstream coverage — restoring a correctness check exactly where the deterministic gates are blind (untested / vacuously-tested paths), at some cost to review focus on low-risk diffs where gate evidence IS present (mitigated by keeping suppression on the evidence path).
- A substance-aware `required_outcomes` is a workflow-spec change — additive within workflow-v1.x if the new outcome/field is optional; otherwise a major bump per the schema-change checklist.
- Customer repos gain an opt-in diff-coverage constraint; no behavior change for repos that do not adopt it.
- The `GateEvidence` plumbing already exists (`prompt.go:2625-2635`), so E46.1 is a prompt-branch inversion plus tests, not new infrastructure.

## Notes

Filed from operator analysis of the implement-review methodology; motivating precedent is run-07bce059 as recorded in `docs/ARCHITECTURE.md:235`. Cross-refs: review lenses `prompt.go:2645-2684`; policy `required_outcomes` `backend/internal/policy/policy.go:214-341`.
