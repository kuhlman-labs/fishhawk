---
id: ADR-038
title: "Add a deploy stage type for post-merge release governance"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/925
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-038: Add a deploy stage type for post-merge release governance

**Status:** **Accepted — 2026-06-27** (reconciled to *delegating-only*; the **agentic** executor mode is split to ADR-039 (#937), deferred). Originally proposed 2026-06-09. Reverses two `MVP_SPEC §9` exclusions and requires a `workflow-v1` bump. Pairs with ADR-035 (own-and-enforce the git contract; orchestrate, do not reimplement) and the review-stage check-gate precedent ("queue and step out of the way").

> **Reconciliation note (2026-06-27):** this body now matches the delegating-only rescope recorded in the comments below — the Decision/Options/Consequences sections previously still read "option 3 (delegating + agentic, phased)". Context stands as originally written. Agentic execution is **not** part of this ADR; it is ADR-039 (#937), deferred until delegating-mode deploy ships and produces usage signal.

## Context

Operators want their deploys governed by the same model Fishhawk applies to code changes: a
human-approvable gate plus a signed, append-only record of what was deployed, where, by
whom, and with what outcome. Today the workflow model stops at merge.

Two things make this a deliberate decision rather than a routine feature:

1. **It is explicitly out of v0 scope.** `MVP_SPEC §9 "Explicitly out of scope"` carries two
   relevant lines: *"Custom stage types beyond plan/implement/review"* and *"Deploy
   monitoring, incident response, rollback orchestration (entirely separate product,
   possibly v2 or never)."* Adding a `deploy` stage reverses both.
2. **It is a closed-set change.** Stage `type` (`plan | implement | review`) and the
   `produces` artifact set (`plan | pull_request`) are frozen closed sets in
   `docs/spec/workflow-v0.md`. Adding `deploy` / `deployment` is a major bump to
   `workflow-v1`, not an additive `v0.x` change.

Operators fall into two shapes:

- Those with an **existing pipeline** (GitHub Actions / Terraform Cloud / Jenkins) who want
  Fishhawk to *trigger it, monitor it, and record the result* under a gate — Fishhawk holds
  no deploy logic and no prod credentials.
- Those wanting an **agentic** deploy where defined skills perform the rollout. *(This shape
  is deferred to ADR-039 (#937); this ADR serves the first shape only.)*

Fishhawk's value here is the **gate + the signed deploy record**, not the deploy mechanics.
The moment Fishhawk holds prod credentials and owns rollback logic it is competing with
Argo / Spinnaker / Terraform Cloud / GitHub Environments and loses its identity as a
governance/workflow envelope (`ARCHITECTURE §1`).

**The structural asymmetry (the core design problem).** Every existing stage produces a
reviewable artifact *before* it takes effect — `plan` before code is written, a PR before
merge — and constraints are evaluated **post-hoc against the produced diff**, so a hit fails
the stage *without anything having shipped*. Deploy breaks this: its effect **is** the side
effect. You cannot evaluate constraints post-hoc and then "reject" — the rollout already
happened. This inverts two things:

- The gate must be **pre-execution** (approve the intent: this sha → this environment, like
  a `plan` gate), not post-hoc like a `review` gate.
- Constraints must be **pre-flight**. `forbidden_paths` / `max_files_changed` are
  meaningless here.

The failure model (`ARCHITECTURE §4`: categories A/B/C/D) also assumes re-execution is safe
and idempotent. A *partially* succeeded deploy is neither, so it needs a `partial` /
`rolled_back` terminal outcome and an explicit rollback sub-action rather than a blind
retry.

## Options

1. **Don't build it — stay pre-merge-only.** *Rejected:* cedes the highest-blast-radius,
   highest-value governance surface; operators are asking for it.
2. **Delegating-only deploy stage** (orchestrate an existing pipeline). **Chosen.** Covers
   the majority (operators with an existing GitHub Actions / Terraform Cloud / Jenkins /
   Argo / Spinnaker pipeline who want it triggered, monitored, and recorded under a gate)
   and is the part that is unambiguously a governance envelope.
3. **Delegating + agentic, phased** (delegating first; agentic later, scoped to ephemeral /
   low-blast-radius environments). **Withdrawn** — the agentic executor mode is split to its
   own ADR-039 (#937), deferred. It crosses credential / irreversibility / identity
   boundaries delegating mode does not, can't be designed concretely until delegating mode
   is real, and is the point where deploy "outgrows the envelope."
4. **A separate post-merge product.** *Rejected as premature:* the governance envelope
   reuses Fishhawk's existing trace/gate/audit machinery; a separate product is warranted
   only if deploy outgrows the envelope.

## Decision

Adopt **option 2** — a *delegating* `deploy` stage in `workflow-v1`.

- New `type: deploy` stage; new `deployment` artifact
  `{environment, ref/sha, external_run_url, outcome, rollback_handle}`.
- **Executor: delegating only.** Fishhawk triggers and monitors an external pipeline; the
  pipeline runs under its own identity. Fishhawk holds **no deploy logic and no prod
  credentials**.
- The deploy approval gate is **pre-execution** (approve the intent — this sha → this
  environment, like a `plan` gate); constraint kinds are **pre-flight**:
  `allowed_environments`, `change_freeze`, `required_upstream` (review merged + `ci_green`).
  Post-hoc diff constraints do not apply.
- The run lifecycle extends past merge (`ARCHITECTURE §4` gains a deploy step after step 7).
- New terminal outcomes for `partial` / `rolled_back`; rollback is an explicit sub-action,
  not a blind retry.
- Fishhawk stays a governance envelope: the delegating mode holds no prod credentials (the
  pipeline runs under its own identity).
- Deploy approval defaults to `all_of` approvers + an SLA (highest blast radius); the
  CLAUDE.md **Auth change checklist** is load-bearing for the dispatch-token scope.
- **Agentic execution (skills perform the rollout) is explicitly out of this ADR**, deferred
  to **ADR-039 (#937)**.

## Consequences

- Reverses two `MVP_SPEC §9` lines — `§9` and `ARCHITECTURE §1/§4` are edited as part of the
  epic (draft wording in the framing comment below, updated to delegating-only).
- `workflow-v1` bump; the backend accepts `v0` and `v1` specs simultaneously during the
  deprecation window. **The v0↔v1 coexistence/routing model is itself an open decision —
  tracked as a dedicated workflow-v1 versioning ADR (filed alongside this reconciliation);
  it is a prerequisite for the first E23.x schema slice.**
- Touches workflow-spec, backend (state machine + persistence + audit kinds), runner (**one**
  executor mode — delegating), CLI, frontend, and the REST API. Narrower than the original
  two-mode surface.
- The pre-execution gate + pre-flight constraints are a new evaluation point distinct from
  the existing post-hoc constraint engine; they do not reuse the diff-evaluation path.
- **Irreversibility is bounded.** Delegating mode never has Fishhawk performing the rollout,
  so the residual risk is "we triggered the wrong intent" — caught by the pre-execution gate
  + pre-flight constraints — not "our agent broke prod." The agentic-execution risk class
  moves with ADR-039.
- Epic #924's **Scope** follows this rescope: the delegating scope stays; the agentic
  Phase-2 lines belong to ADR-039 (#937) and are gated on it.

Parent epic: #924
