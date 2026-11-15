---
id: ADR-035
title: "Own and enforce the git contract of a run (declared base + sole-writer branch lineage)"
status: accepted
date: 2026-06-07
issue: https://github.com/kuhlman-labs/fishhawk/issues/857
supersedes: []
superseded_by: []
amended_by: ["ADR-141"]
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

## Decision

**Accepted (2026-06-07).** Status confirmed by the Captain (repository maintainer) on 2026-10-23 from the record's own source: "**Status: Decided (2026-06-07).**".

## Amendment A1 (accepted 2026-11-14) — an operator-vouched commit is an attributed exception to the sole-writer rule

This amendment recognises one exception to rule 2 and states how rules 3 and 4 apply to it. It adds no verification requirement.

**The rule it qualifies.** Rule 2 says "the sanctioned runner is the only writer of the run branch" and "A commit not authored by this run is a contract violation." The operator's vouch already admits a commit the runner did not write. `fishhawk_vouch_commit` records an `operator_commit_vouched` audit entry naming the run, the full commit SHA and the operator's reason (`backend/internal/server/vouch.go:21-38`, `:52-59`). The reported-head ledger then counts that SHA as run lineage (`backend/internal/server/lineage.go:653-669`). This amendment makes that exception part of the contract.

**The exception.** A commit on a run branch that an operator has vouched is attributable to the run by that declaration. It is not a contract violation under rule 2, and rule 4's halt does not apply to it. The exception is **attributed**: the audit entry records the declaring subject as its actor, with the reason (`vouch.go:306-334`).
- **Who may vouch.** A vouch is made on an operator credential that carries `write:stages`. A run-bound agent token is refused, even for its own run (`vouch.go:210-221`, `:222-236`). This amendment does not place the vouch on an ADR-148 tier, so it keeps that guard.
- **What it proves.** A vouch attributes; it does not verify. The record is kept for any well-formed full SHA, whether or not the commit is on the branch (`vouch.go:137-147`, `:173-177`). ADR-116 says the generic vouch "verifies nothing" (`docs/adr/116-fan-in-conflict-recovered-in-place.md:114`). Rule 3 is therefore not met by a vouch. The exception relaxes rule 3 for attribution only: lineage and landing are still checked against VCS facts.

**Under ADR-090 D8's conditions.** A vouch does not by itself exempt a head from merge-candidate verification. ADR-090 D8, as amended, says: "Operator-vouched heads are not gated ONLY when they bring in no base commit the newest verified head lacks." An operator base merge or conflict-resolution merge is a base advance and needs a passing merge-candidate verify (`docs/adr/090-verify-the-merge-candidate.md:224`). For a queue-dispatched merge ADR-093 governs in D8's place (`docs/adr/093-fishhawk-owned-merge-queue.md:67`): the queue merges only a head with a verified pass (`:72`). A voucher also counts as a change author wherever a forge approval is checked under ADR-112 R1(b): queue, inline and delegated merges alike (`docs/adr/112-queue-merge-forge-approval-predicate.md:65-67`, `:145`). A vouch also breaks R1's clean-advance approval chain (`:53`).

**No new verification requirement.** This amendment requires no proof of authorship and no added check. ADR-090 rejected a universal rule that every live head be gate-verified, because "It would wedge existing vouch, park and held-commit flows" (`docs/adr/090-verify-the-merge-candidate.md:112`). ADR-141 D5 does not authorize automatic vouching (`docs/adr/141-forge-confirmed-push-reports.md:41`), and neither does this amendment.

**PR adoption.** An ADR-146 family 2 run carries a pull request head that its own agent did not write (`docs/adr/146-workflow-catalogue.md:64`). How an adopted head's commits are attributed to the run is part of ADR-146 Direction (a): its head and base binding, and an agent's change to an adopted pull request (`:95`). This amendment does not decide it. It neither extends the vouch to adopted heads nor rules that out.

**What stands.** Rule 2 stands for every commit that is not vouched. Rules 1 and 5 stand. Rules 3 and 4 stand except as this exception states: an unvouched foreign commit is still a contract violation and still halts the run. ADR-141's report-ingestion rules stand.

- **Record metadata:** an in-body amendment of ADR-035. No new edge. ADR-035 keeps `amended_by: ["ADR-141"]` and `status: accepted`.
