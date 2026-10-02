---
id: ADR-080
title: "Solo-captain alpha: the repository is the unit, one developer commands the full crew locally from the bridge"
status: accepted
date: 2026-09-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/3693
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-080: Solo-captain alpha: the repository is the unit, one developer commands the full crew locally from the bridge

## Context

The charter (§1–§2 before this ADR) defines alpha as ADR-057 Mode 1 working for an external team: they self-host on their own Kubernetes cluster and run governed changes on GitHub or GitLab. `MVP_SPEC.md` §2 names a 50–300-engineer, compliance-conscious organization as the design-partner ICP, with engineering leadership as the buyer; `BRAND_FOUNDATIONS.md` §4 addresses the same buyer.

Three observations from the code and the dogfood record argue that this is the wrong first customer and the wrong first shape:

1. **The product already works as a solo-operator product.** This repository — more than 1,600 pull requests, one human — is built under Fishhawk. `METHODOLOGY.md` records that the `feature_change` escalation deliberately raises no approval count because "this repository has one eligible approver." The strongest evidence for the product is one person running a repository the way a team would.
2. **The control surface already fits a "captain and crew" model.** Autonomy is declared per workflow (`autonomy: low|medium|high`, ADR-066) and expanded to an action matrix; `escalations` clamp `max_autonomy` per path; `page_human_on` sends disagreement arbitration to the human at every tier; `clarification_request` requires a `recommended_default` and `tradeoffs`; the operator agent acts only within delegation and pages with a distilled hand-off; the charter and `.fishhawk/**` are read from the base ref and forbidden to implement agents.
3. **What is missing is not the engine but the rest of the crew and the bridge.** The loop covers plan → implement → review → merge. The work a team does before an issue exists and after a merge — direction, release, post-deploy verification, upkeep, security, feedback, and remembering why the repository is the way it is — is partly in flight (E71, E33, E35) and partly absent. The Web UI is a run-record browser; E40 (attention queue, repo dashboard, decision write-surface) is parked in beta with 0/6 done, and the charter explicitly accepts that "a non-engineer approver cannot act on a gate."

A related gap: nothing reads the audit chain back. Every gate decision is recorded; no surface shows a later captain — or the crew — how similar calls were judged. ADRs live only as issues (E15 intended `docs/adr/`; it never landed). A repository run by one person is only as durable as that person's memory unless doctrine and precedent live in the repository.

### Vocabulary

| Term | Meaning in Fishhawk |
|---|---|
| Captain | The human who commands a repository: ratifies direction, sets delegation, decides at gates. A role, not a person — resolved through the existing approval predicates (`min_permission`, `member_of`, `not: [author, agent]`). |
| Crew | The agent roles that do the work. Addressed by role, never by model or instance. |
| First officer | The operator agent (ADR-040). |
| Standing orders | `.fishhawk/charter.md`, `.fishhawk/workflows.yaml`, `.fishhawk/operator.yaml`, and the in-repo decision record. |
| Ship's log | The signed audit chain. |
| Bridge | The Web UI surfaces where the captain reads state and decides. |
| Fleet | Many repositories, each with a captain. The enterprise case. Not an alpha concern (N7 still holds). |

The vocabulary is internal and in product copy uses generic nautical terms only; no franchise names or imagery.

## Options

1. **Keep the current alpha.** Finish E64 (Mode 1 on Kubernetes for an external team; 59/82 children done; two V1 items left, #2032 and #3187). Earliest external milestone; aims at a customer who needs SSO, quorum, and procurement answers before the crew and the bridge exist.
2. **Solo-captain alpha, full crew, local.** Redefine alpha as one developer running the whole stack locally and commanding the full crew from the bridge, with handover to a second captain demonstrable. Move Kubernetes, hosted, and multi-tenant work to beta. Largest alpha by scope; aims at the customer the product already serves.
3. **Solo-captain alpha, core crew only.** As option 2, but alpha requires only the shipped loop plus the bridge and local install; the added crew roles land in beta. Fastest to a solo-developer release; the "full crew" claim waits.

## Recommendation

Option 2, with two guards on scope:

- **Cut the current state first.** No version tag has ever been cut. Tag a `v0.1.0` preview of today's local path (with the E33 release loop, which is 5/6 done) before the alpha definition changes, so the redefinition does not hold back anyone who wants to try the loop now.
- **Define "ready" per role, not per feature list.** Each crew role reaches alpha when it runs as a governed workflow on this repository and on one repository that is not this one, meeting a one-line done-means (charter §2 table). Depth beyond that is beta.

Earned autonomy (the crew's record recommending delegation changes, human-ratified) is designed in alpha and may land after it. It changes delegation, the most safety-relevant surface, and should not be rushed onto the alpha critical path.

GitLab: the forge support stays shipped (E45, 87/100), but its live walk (#2032) leaves the alpha critical path. Alpha needs one forge proven for a solo developer; forge agnosticism is re-proven at beta against a design partner.

## Decision

**Accepted (2026-09-26).** Option 2 (solo-captain alpha, full crew, local), with both scope guards: cut a `v0.1.0` preview of today's local path before the redefinition takes effect, and define readiness per crew role (charter §2 done-means table). Ratified by the maintainer on this issue on 2026-09-26.

Enacted by:
- **Tracker:** alpha tracker E73 #3694; new epics E74 #3695, E75 #3696, E76 #3697, E77 #3698, E78 #3699, E79 #3700, E80 #3701, E81 #3702, E82 #3703 (design in alpha, `milestone:beta`). The Consequences milestone moves were applied as label edits on 2026-09-26; E64 #2308 carries a dated amendment and keeps its title pending a maintainer decision; the E60 #2289 split is proposed on that issue.
- **Charter:** #3705 amends `.fishhawk/charter.md` §1, §2 and rubric V1/V2.
- **Docs:** `MVP_SPEC.md`, `BRAND_FOUNDATIONS.md`, `README.md` and the site landing/introduction land through a governed run parented to E73.

## Consequences

**Charter.** §1 north star and §2 alpha definition are rewritten (human-authored, per charter §6). Phase themes T1–T6 are retired and T7–T12 added; rubric line V1's alpha example and V2's theme range (now T7–T12) are updated; no rubric id changes meaning. The groomer ranks against the new definition from the first run after the amendment merges.

**Milestones.** Proposed moves (each a human edit to the epic's labels):

| Epic | Today | Proposed | Why |
|---|---|---|---|
| E40 #1712 Frontend IA (the bridge) | beta | **alpha** | T8 |
| E71 #3240 Product manager | alpha phase, beta milestone | **alpha** | Crew role |
| E35 #1585 Post-deploy verification + incident intake | beta | **alpha** | Crew role; ADR-053 #1581 already accepted (2026-07-26), so unblocked |
| E55 #2241 Repo-declared review conventions | beta (phase alpha) | **alpha** | Document-injection foundation for the architect and crew messages |
| E33 #1583 Release governance | alpha | alpha | Crew role; also cuts the v0.1.0 preview |
| E54 #2232, E72 #3324 | alpha; E72 unset | alpha | Crew roles |
| E60 #2289 Beta hardening | alpha + beta | split: gate notifications **alpha**, per-tenant caps **beta** | The captain needs pages; tenancy does not apply locally |
| E62 #2299 K8s substrate, E69 #2911 local K8s friction | alpha (E62 also beta); E69 unset | **beta** | No Kubernetes in alpha |
| E61 #2297 BYOK, E66 #2388 MCP over HTTP | unset (phase alpha) | **beta** | Satisfied or unneeded locally |
| E45 #1852 GitLab | alpha | alpha code, **#2032 walk to beta** | One forge proven for alpha |
| E64 #2308 Alpha tracker | unset (phase alpha) | **beta**, retitled "Mode 1 self-host for an external team" | Its open V1 items are re-triaged: #2032 moves with it to beta; #3187 (live injection eval) stays alpha as R2 |

**New epics.** E73 (alpha tracker), E74 (local install), E75 (historian), E76 (change of command), E77 (crew communication), E78 (architect and decision record), E79 (chief engineer), E80 (security officer), E81 (comms), E82 (earned autonomy — design in alpha).

**Docs.** `MVP_SPEC.md` §1–§3, `BRAND_FOUNDATIONS.md` §1, §3 and §4, `README.md`, and the docs site landing and introduction are revised to the new direction. `METHODOLOGY.md` is unchanged: it describes how this repository is built, which is already the solo-captain case.

**Unchanged.** Non-goals N1–N8. The closed stage set (N6): new crew roles are reviewer personas, scheduled or event-triggered workflows that file work, or read paths over the chain — not new stage types. No crew role acts outside a governed run: proactive roles file issues into the same intake → groom → plan → gate loop.

**Risk.** Alpha grows from two open V1 items to roughly ten epics, eight not started. The v0.1.0 preview and the per-role done-means are the mitigations; if T9 stalls, option 3 remains available as a fallback without reopening this ADR's direction.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":1915} -->
