---
id: ADR-035
title: "Own and enforce the git contract of a run (declared base + sole-writer branch lineage)"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/857
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-035: Own and enforce the git contract of a run (declared base + sole-writer branch lineage)

**Status: Decided (2026-06-07).** Establishes a new run-integrity invariant. Composes with **ADR-031** (#710, "review succeeds iff verifiably landed") and the **invariant monitor** (#837); extends the same *verified-not-asserted* philosophy from *landing* to the *branch lineage* itself.

## Context

Fishhawk vouches for the **trace** of a run — signed trace bundle, tamper-evident audit chain, backend policy re-eval — but it does **not** vouch for the **git branch** the trace claims to describe. The harness asserts the trace, not the tree.

Run branches are namespaced by convention (`fishhawk/run-<id>/stage-<id>`), but namespacing is a convention, not a guarantee: nothing enforces exclusive ownership of the branch, and nothing on the backend asserts that the branch's actual base/HEAD lineage matches what the run authored.

**Incident that motivated this (run `74e8eade`, #797 / PR #855, 2026-06-07):** a second agent (Codex) started a run and committed onto the branch *this* run had created. That foreign commit (`509a62c`) became the PR's `base_sha`; the run's own implement commit (`3acc640`) stacked on top, and Codex's change rode **silently** into the PR diff. It surfaced downstream as a false "scope drift" finding, a wasted fix-up, and a stranded run (the fix-up no-op-hang, #856). Every Fishhawk artifact (trace, audit, policy) was pristine — while the branch underneath was contaminated by a writer the harness never accounted for.

The runner's `pull_request` / `fixup_pushed` reports already carry `head_sha` and `base_sha`; the backend records them but never asserts lineage. Half-ownership — runner commits, operator/reconciler merges, no component asserts branch integrity — is exactly where this class of bug lives (#762 / #780 / #784 / #818 / #823 / #856 all share loose branch-state assumptions).

## Options

1. **Reimplement git flow inside Fishhawk** (own worktrees, rebase, conflict resolution, merge strategies, force-push semantics). **Rejected:** a huge, sharp surface that duplicates git + GitHub, works against platform-neutrality (ADR-031) and MVP focus, and discards branch-protection / PR review, which are load-bearing for human trust.
2. **Status quo — naming convention only.** **Rejected:** this incident proves convention ≠ guarantee.
3. **Own and ENFORCE the git *contract* of a run — orchestrate-and-verify, not reimplement.** **Chosen.**

## Decision

A run **declares**, and Fishhawk **enforces**, the git contract of its branch:

1. **Declared base.** At branch creation the run records the `base_sha` it cuts from, as an immutable run fact.
2. **Sole-writer attribution.** Every commit in `(declared_base, reported_HEAD]` must be attributable to *this run's* stages — the sanctioned runner is the only writer of the run branch. A commit not authored by this run is a contract violation.
3. **Verified, not asserted** (mirrors ADR-031). Enforcement keys off real, queryable VCS facts (base/HEAD lineage + commit attribution), never an operator's or agent's word. The check runs at the boundaries the backend already ingests `head_sha`/`base_sha` (the `pull_request` / `fixup_pushed` reports) and is surfaced through the #837 invariant monitor.
4. **Fail loud, never silent.** A contaminated branch halts the run (or fails the stage) naming the offending commit. It must NEVER ride silently into the PR diff or a downstream review.
5. **Compose with GitHub, do not replace it.** This is an integrity invariant layered over GitHub's branch / PR / branch-protection / merge model, not a substitute. ADR-031's "`succeeded` iff verifiably landed" plus this ADR's "the branch contains exactly what this run authored" together close the loop: the harness vouches for both the **trace** and the **tree**.

**Scope boundary (the load-bearing part of this decision):** *orchestrate-and-enforce* — declare the base, verify lineage + authorship — **NOT** reimplement git. The first deliverable is the base/HEAD-lineage + sole-writer guard.

## Consequences

- **Positive:** closes the trace-vs-tree integrity gap; converts silent branch contamination into a loud, named halt; gives branch-integrity checks a home (the invariant monitor); and prevents the false-drift → wasted-fix-up → stranded-run chain this incident produced.
- **Cost / design surface for the impl issue:**
  - **Attribution predicate** — how the backend knows a commit was "authored by this run": compare `(base..HEAD]` against the set of `head_sha`s this run's stages reported, and/or sign run commits (trailer / verified committer identity). Needs a precise definition.
  - **Enforcement point** — report-time gate vs a pre-merge invariant vs both.
  - **Remediation** — halt + notify vs auto-reset to the last run-authored HEAD.
  - **Concurrency** — prevent two runs (or a foreign agent) from targeting the same branch namespace in the first place (ownership at creation, not just verification after).
- **Cross-refs:** ADR-031 (#710), invariant monitor (#837), #856 (fix-up no-changes hang), fix-up lineage #762 / #780 / #784 / #818 / #823.

Parent epic: #389
