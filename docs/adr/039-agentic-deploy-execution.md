---
id: ADR-039
title: "Agentic deploy execution (skills perform the rollout)"
status: unknown
date: 2026-08-25
issue: https://github.com/kuhlman-labs/fishhawk/issues/937
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-039: Agentic deploy execution (skills perform the rollout)

**Status:** Proposed — 2026-06-09. Split out of **ADR-038** (#925), which is rescoped to *delegating-only*. **Deferred:** not to be decided until delegating-mode deploy (epic #924) is real and has produced usage signal.

## Context

ADR-038 adds a *delegating* `deploy` stage: Fishhawk gates the deploy intent, triggers + monitors an external pipeline, and records a signed `deployment` artifact — holding **no prod credentials and no deploy logic**. This ADR asks the separate question ADR-038 deliberately excluded: should Fishhawk also support an **agentic** deploy executor, where agent skills perform the rollout directly rather than delegating to an external pipeline?

Agentic deploy is *surface-consistent* with Fishhawk's existing executor model — agents already perform `plan` / `implement` work under gates — but it crosses three boundaries delegating mode does not, which is why it is a separate decision rather than a phase-2 of ADR-038:

1. **Reversibility.** A rollout's effect *is* the side effect; there is no post-hoc reject. Agent error costs a prod incident, not a rejected PR. (`plan`/`implement` produce reviewable artifacts before they take effect; deploy does not.)
2. **Credentials.** Fishhawk would have to hold or broker **prod infrastructure credentials** — a categorically larger trust/security surface than the repo-scoped GitHub App tokens implement uses.
3. **Identity.** An agent that performs rollouts *is* an agentic CD tool — the exact line `ADR-038` / `ARCHITECTURE §1` draw as where Fishhawk stops being a governance envelope and starts competing with Argo / Spinnaker / Terraform Cloud.

## Options

1. **Never build it.** Fishhawk stays delegating-only for deploy permanently; agentic rollout is explicitly someone else's product.
2. **Agentic executor mode inside Fishhawk**, scoped to ephemeral / low-blast-radius environments first, with short-lived narrowly-scoped credentials and pre-flight constraints before any prod target.
3. **A separate product** built on (but not inside) Fishhawk's governance envelope — the gate + signed record is the reuse boundary.

## Recommendation

**Defer.** Do not decide until delegating-mode deploy (epic #924) ships and produces real signal on: (a) whether demand for agentic rollout is real, (b) what credential-custody model it would require, and (c) whether the governance-envelope identity holds or breaks under it. Revisit per `BRAND_FOUNDATIONS §2` ("v0 is governance and v1+ is full SDLC orchestration").

## Decision

**Deferred. Charter N4 is the standing answer until this ADR is revisited.**

Charter §3 N4 states: *Fishhawk does not own deploy mechanics. The governed deploy* gate *and the signed record are in scope (ADR-038); pipeline logic, production credential custody, and rollout execution are not.* Rollout execution is therefore **out of scope today**, which is Option 1 of this ADR in force by default — not by omission.

This ADR remains OPEN because it is the vehicle for *revisiting* N4 should the deferral trigger fire, **not a parallel track around it**. Nothing here authorizes work toward Option 2; a change of position requires amending N4 per charter §6, which is a founder decision and not an ADR side effect.

Recorded 2026-08-25 after backlog grooming run `d650b22a` flagged this item as vision drift against N4 (`basis: non_goal`). The flag was correct that the item sits on the far side of the N4 line; what it could not see is that `Status: Proposed` with an empty Decision section reads as live design space to a grooming run and to any agent, when in fact the question was already deferred with a concrete trigger. This section removes that ambiguity. The deferral, the revisit trigger, and Option 1's standing status are unchanged by this annotation — only their legibility is.

## Consequences

- Keeps ADR-038 small and shippable (delegating-only), and keeps its irreversibility risk bounded to "wrong intent triggered" rather than "our agent broke prod."
- The `deployment` artifact and the pre-execution-gate / pre-flight-constraint model from ADR-038 are designed to **not** depend on executor mode, so adopting an agentic mode later (option 2) is additive, not a re-architecture.
- If option 3 (separate product) is chosen, the governance envelope (gate + signed record) is the explicit reuse boundary.

Parent epic: #924
Related: ADR-038 (#925)
