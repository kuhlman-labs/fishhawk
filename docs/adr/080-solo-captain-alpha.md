---
id: ADR-080
title: "Solo-captain alpha: the repository is the unit, one developer commands the full crew locally from the bridge"
status: accepted
date: 2026-09-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/3693
supersedes: []
superseded_by: ["ADR-099"]
amended_by: ["ADR-130"]
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

**Partially superseded (clarified by the Captain 2026-10-23):** Partially superseded by ADR-099 (glossary row 'First officer = the operator agent'). The rest of ADR-080 still governs.

## Amendment A1 (accepted 2026-11-14) — phase scoping: what alpha keeps, defers to beta and retires

This record moved hosted, multi-tenant and Kubernetes work to beta (Option 2, `:44`; the milestone table, `:81-85`). It moved epics, not obligations. This amendment states, obligation by obligation, what that move does to the records concerned. It decides nothing in those records, except the ADR-056 row's alpha documentation rule and the ADR-071 row's interim timer rule, which are this amendment's own. Every other row names the record that decides it.

**The three kinds of row.**
- **Kept in alpha.** A safeguard that binds in alpha. The move to beta does not relax it.
- **Deferred to beta.** Delivery first required at beta, in the `milestone:beta` sense: the earliest milestone that requires the work. Delivery is not an alpha release requirement; existing implementations and retained safeguards remain governed by their records.
- **Retired.** A design that no phase delivers. It is not a beta delivery.

**The phase table.** In this amendment, `NNN:L` cites line L of the ADR-NNN record in `docs/adr/`, and a bare `:L` cites this record.

| Kind | Record | Obligation | Decided by |
|---|---|---|---|
| Kept in alpha | ADR-054 | The compliance export's four postures: a dedicated `read:audit-export` scope, a redacted default with raw pointers, run-less entries partitioned and never dropped, and a partial export that declares itself and resumes (`054:36-39`). | ADR-054; built under E9 #9 (`backend/internal/server/audit_export.go:54`). |
| Kept in alpha | ADR-056 | No alpha document or product surface claims an anchored head or tamper-proofness; until anchoring ships an export shows internal consistency only (`056:16`). | This amendment, applying ADR-056's trust-model discipline (`056:64`). |
| Kept in alpha | ADR-057 | The per-account audit chain (decision 2, `057:46`; shipped, `056:38`), the account scoping already built, and the sign-in admission gate (`057:39-41`; Amendment A2, `057:124`). | ADR-057, and ADR-147 D2: "Until then ADR-057's decision stands for the code already built." (`147:64`). |
| Kept in alpha | ADR-062 | Its fail-closed guards. An unset cell region disables the pin surface (A2.4, `062:71`; `backend/internal/server/regionpin.go:48-49`). The directory's assignment endpoint is gated before any deployment (A2.5, `062:73`; `directory/pkg/routing/router.go:295`). The credential-withholding guards stay until their execution paths are retired. | ADR-062 A2; ADR-147 D5 (`147:86`). |
| Kept in alpha | ADR-071 | Decision 1, asymmetric intake sanitization (`071:76-85`; E60.1 #2290, E60.2 #2291). The gate notification surface decision 2 adds, whose notifications degrade to advisory and never block a gate (`071:110`; #2292). | ADR-071; this record's E60 split (`:81`), as applied on E60 #2289 (2026-09-26). |
| Kept in alpha | ADR-071 | An SLA breach does not fail a stage (ADR-071 §2, `071:89`). Until the optional timer conforms (#5310), it stays off in the default configuration an alpha user runs (`backend/cmd/fishhawkd/serve.go:1973-1975`). Secondary-approver escalation remains deferred to beta (#2293). | ADR-071 (`071:89`); the interim timer rule is this amendment's own and ends when #5310 lands. |
| Kept in alpha | ADR-072 | BYOK: all inference runs on the operator's own key, and Fishhawk buys no tokens (`072:73`). Decision 3's deployment allow-list, checked up front (`072:91`, `072:93`; `backend/internal/server/approvals.go:2873`). | ADR-072; ADR-147 D2: "BYOK itself is unchanged." (`147:62`); charter §2 D3 (`.fishhawk/charter.md:120`). |
| Deferred to beta | ADR-056 | Anchoring itself: signed checkpoints replicated into every repository of the account (`056:48-52`, `056:50`), the verifier's anchored-head check and the Rekor opt-in (`056:34`). It is sequenced after E44 (`056:58-60`). | ADR-056; E56 #2247. |
| Deferred to beta | ADR-057 | Hosted Mode 2 (`:44`; charter §2 D2, `.fishhawk/charter.md:119`), and Mode 1 for an external team (`:85`). This includes the RLS backstop active in production (`057:56`, `057:77`; #2083), the Captain's re-acceptance or change of pooled isolation on ADR-147 D2's restated basis (#5287), and D2's three storage obligations (#5288). | This record (`:44`, `:85`); charter §2 D2 (`.fishhawk/charter.md:119`); ADR-147 D2 (`147:63-64`) and its beta-entry obligations (`147:124`). |
| Deferred to beta | ADR-058 | The live GitLab walk #2032. The GitLab code stays shipped (`:56`, `:84`). Whether beta needs a second forge stays open in E57 #2255. | This record (`:56`); charter §2 D7 (`.fishhawk/charter.md:124`). |
| Deferred to beta | ADR-062 | Regional inference and the binding of runner, model endpoint and region, which dispatch checks (#5290). The directory and region-scoped cells as deployed, and the per-region deploy topology (`062:57`). | ADR-147 D5 for regional inference (`147:83-85`), with its beta-entry obligations (`147:124`); for the directory, cells and topology, this record (`:44`) and charter §2 D2 (`.fishhawk/charter.md:119`). |
| Deferred to beta | ADR-071 | Decision 2's escalation to a secondary approver (`071:89`; #2293). Decision 3's per-account caps (`071:93-99`; #2294). The partner-scenario capstone #2295. All three are under E84 #3715. | ADR-071; this record's E60 split (`:81`), as applied on E60 #2289 (2026-09-26). |
| Deferred to beta | ADR-072 | Customer-actionable provider failures, including the failure message that says which side refused (`072:93`), and the onboarding step a customer key adds (`072:112`, `072:115`). The deployment allow-list's refusal is kept above. | ADR-072; this record's E61 move (`:83`). |
| Deferred to beta | ADR-075 | The Kubernetes runner backend in full: the pull controller, the runner image and run-scoped forge tokens (`075:81-109`). | ADR-075; this record: "No Kubernetes in alpha" (`:82`); E65 #2311. |
| Retired | ADR-072 | Per-account encrypted storage of customer model keys on the control plane (decision 1, `072:77`; its binding constraint, `072:87`; `072:111`). | ADR-074: "E61's per-account credential storage is deleted" (`074:67`). ADR-147 D2: a hosted control plane holds no model key once E63 lands (`147:62`). |
| Retired | ADR-072, ADR-062 | Control-plane custody of a customer's key in per-cell configuration (`072:77-79`), the backend-side review that uses it (`072:81-85`), outside ADR-147 D1's local exception; and the per-cell reviewer key of a hosted cell's inference configuration (`062:43`). | ADR-147 D1 replaces ADR-072 decision 2. D2 ends decision 1's custody on hosted and customer-installed deployments (`147:62`, `147:105`) and ends the hosted cell's reviewer key (`147:111`). |

The local alpha deployment is ADR-147 D1's single-tenant local exception (`147:56`). There the local fishhawkd still holds the operator's own model keys, as a stated residual (`147:118`). Where a runner obtains a customer's key is outside this table, as ADR-147 D2 leaves it (`147:62`).

**ADR-013's licence trigger is unchanged.** BSL is applied to the designated paths when the first designated-enterprise surface ships to a paying customer, "not on a date, and not on a feature merely existing", and the trigger is re-reviewed with counsel when it fires (`docs/adr/013-apache-bsl-enterprise-boundary.md:47`). It is an event, not a phase. Neither the end of alpha nor beta entry fires it.

**Alpha scope.** Fishhawk stays free, Apache-2.0 and local-only through the solo-captain alpha, with no billing. Local-only describes what an alpha user is offered: the user runs the stack on their own machine (charter §2, `.fishhawk/charter.md:56-61`), and no hosted deployment is offered to an alpha user as the product. The revisit triggers that go with this scope are in ADR-010, ADR-011 and ADR-012 (`docs/adr/010-marketplace-billing-path-github-vs-direct.md:33`, `docs/adr/011-pricing-model-per-engineer-vs-per-run.md:33`, `docs/adr/012-design-partner-sourcing-strategy.md:34`).

**Readiness check before the first external alpha user.**

*Who counts.* An external alpha user is a person other than this repository's Captain who runs Fishhawk on a repository of their own. Onboarding is the Captain's act: inviting or supporting that person, or announcing a release as the alpha for outside users. A person who runs the public source unprompted is not onboarded. A later captain of this repository is not an external user.

*What the Captain checks.* Before the first such user is onboarded, the Captain checks three things against the release that user will install:
1. every "Kept in alpha" row of the phase table holds;
2. the release and onboarding offer conform to the alpha scope statement above, and the user has received that statement;
3. the user has been told, in plain words, of the single-host residuals they accept:
   - ADR-147 D1's local exception is declared, not enforced (`147:59`);
   - the residuals ADR-147 and ADR-148 state for the local stack (`147:118`, `148:124-125`);
   - under ADR-149 D5, a local run that is not charter-driven, attended or unattended, keeps detection-only confinement (`docs/adr/149-confinement-before-charter-operation.md:79`).

*Where it is recorded.* The Captain records the result in a comment on the alpha tracker, E73 #3694 (`:63`). The comment gives each item's result with its evidence, and names the release checked. An agent may gather the evidence; the Captain posts the comment under their own forge identity. Onboarding waits until every item passes.

*Relation to other conditions.* This check is in addition to any condition another record sets on a first external user (ADR-067, `docs/adr/067-workflow-spec-grammar-consolidation.md:51`); it replaces none.

*What it is not.* The check is a requirement, not a product feature: this amendment builds no verb, gate or workflow for it. It is not an action ADR-148 places, so it sits on no tier: "An action this record does not place keeps the guard its governing record states, and is not thereby on a tier." (`docs/adr/148-human-authority-ladder.md:48`).

**What stands.**
- This record's Decision, its alpha definition and its milestone table (`:60`, `:71-85`).
- The changes ADR-099 and ADR-130 made to this record (`:95`; `docs/adr/130-cross-repository-project-coordination.md:79`).
- This record's status. The amendment adds no phase field to the record index.
- Each row is decided by the record it names. Where that record is later amended, the amendment governs, not this table.

- **Record metadata:** an in-body amendment of ADR-080. No new edge.
