---
id: ADR-134
title: "Retirement governance and separate data disposition authority"
status: accepted
date: 2026-11-04
issue: https://github.com/kuhlman-labs/fishhawk/issues/4900
supersedes: []
superseded_by: []
amends: []
applies_to: []
---

# ADR-134: Retirement governance and separate data disposition authority

**Status: accepted (ratified 2026-11-04).**

## Context

Prepared at the Captain's request on 2026-10-31. The delivery lifecycle needs a governed exit as well as a release path. Removing an API, component, service, feature flag, environment, or data store can affect consumers after the code is merged. Zero recorded use does not establish zero consumers, and shutting down a service does not authorize deleting its retained data.

Existing decisions handle particular removals and compatibility rules: ADR-073 decides to retire the ECS path, and the three Terraform files it names for deletion remain (`docs/adr/073-kubernetes-helm-single-deployment-substrate.md:93`, `infra/terraform/README.md:144-145`); ADR-046 preserves workflow majors until a separate sunset decision; ADR-026 keeps both schemas live during a deprecation period (`docs/adr/026-backward-compatibility-for-schema-extension-prs.md:54`). Existing campaigns and change/deploy workflows can carry the work, but there is no common retirement evidence and authorization contract.

## Options

- **A. Treat retirement as an ordinary delete-code issue.** Simple, but cannot establish consumer readiness, external removal, or data authority.
- **B. A versioned retirement dossier carried by ordinary work, with separately gated removal and data disposition (recommended).** Reuses existing tracker, campaign, approval, deployment, and audit surfaces.
- **C. A new retirement stage type and long-running lifecycle engine.** Deferred: retirement often spans several releases; a perpetual run or new workflow type adds little authority or evidence.

## Recommendation

Adopt option B. The initial dossier is a reviewed, versioned repository document at a path the repository declares on an ADR-110 rule-1 configuration surface (`docs/adr/110-repository-agnostic-product-features.md:58`), identified by repository, path and git blob SHA on the default branch; work items, approval statements and removal intents bind that blob SHA. It is not a new backend state machine. The dossier contains only metadata approved for its repository's visibility. Sensitive consumer records, raw telemetry, retention instructions and security evidence remain in appropriately access-controlled storage; the dossier carries authorized opaque references and redacted summaries. References grant no additional read authority, and inaccessible evidence remains unknown. Security findings follow ADR-126 rather than ordinary repository or tracker storage. A future typed API/artifact would require its own implementation design and schema review.

### D1. Define exactly what is being retired

The dossier names the capability/resource identity, owner, supported versions and consumers, reason, replacement, desired end state, known dependencies, evidence sources, proposed dates/windows, removal criteria (each marked required or optional), migration path, notice requirements, risk owner, and recovery/abandonment conditions; D2 approves them as part of the revision. It distinguishes deprecation (supported but discouraged), disabled operation, removal, archive/retention, and deletion. A component replacement is not automatically a retirement of its stored data or published historical artifacts.

Consumer evidence includes measured usage, declared dependents, offline or infrequent clients, external integrations, and inaccessible/unknown populations. Measurement windows and blind spots are stated. "No observed use" is only evidence about the observed population and period. A deadline does not automatically authorize removal.

### D2. Human approval establishes the retirement commitment

The commitment covers the evidence basis, affected audiences, migration path, notice requirements, removal criteria, risk owner, and recovery/abandonment conditions. The approver admitted by the repository's declared approvals predicate (D5) approves the exact dossier revision, which carries them and is identified by its declared path and blob SHA; the approval names that blob SHA and each later authorization names it in turn. No product surface records it yet. In initial delivery each authorization is an act the admitted identity performs on the forge, a pull-request review approval or a comment by that identity on the retirement issue, that names the dossier blob SHA; such a comment is evidence the operator cites, not the issue-comment gate channel ADR-123 Decision 4 refuses, and it advances no stage gate. The forge's authentication of the author is the verification. The operator's record cites that act by its forge URL, copies its text and the forge's last-edited time into the record, and records the blob SHA of the approvals-predicate declaration it was evaluated against, read from the default branch; an act edited after it was cited is a new act. A line an operator or agent writes into a document is not an approval. A product surface belongs to the removal-dispatch ADR (D5). Binding by SHA already means a later edit to the dossier is not what was approved, so no `protected_paths` declaration is required for the dossier itself. The file that declares the approvals predicate and the dossier home is different: it decides who may authorize, and it is not covered by that binding. Until ADR-121's `protected_paths` is implemented, the repository lists it in the implement stages' `forbidden_paths`, and the operator checks that before relying on it; the removal-dispatch ADR binds it under ADR-121. Subsequent changes to scope, target, criteria, or data handling require a new decision. Existing supported-version or compatibility promises remain governing: retiring a workflow major still requires the separate sunset ADR specified by ADR-046.

This generic ADR supplies procedure; it does not sunset any version. A retirement that changes architectural or public compatibility commitments needs its own ADR, ratified under ADR-113, before any removal intent is authorized.

### D3. Preparation is a campaign of ordinary changes

Use ADR-052 refinement and ADR-047 campaigns to deliver replacement behavior, adapters, telemetry, migration tools, documentation, and consumer migration. Work items reference the dossier revision and acceptance criteria. Existing feature/routine/human-led workflows apply according to scope; there is no path around the repository's declared `forbidden_paths`, nor, once ADR-121's `protected_paths` declaration (`docs/adr/121-protected-judge.md:81`) is implemented, around its protected paths.

Notices and customer communications remain operator-owned external actions. Record the communication evidence and intended audience; an agent-authored draft is not a sent notice or a proof that all consumers received it. The campaign may span releases without leaving one run parked for months.

### D4. Assess readiness immediately before removal

Produce a dossier-bound assessment of migrated consumers, remaining usage, notice evidence, replacements, dependency state, retention obligations, recovery artifacts, and operator ownership. Where an agent drafts it, consumer declarations, telemetry exports and cross-repository evidence enter its prompt only as quarantined data, and each criterion's passed/failed/unknown mark cites a structured source rather than prose. The assessment names its producer role and is recorded separately from the approved dossier, never inside it, because an edit to the dossier changes the blob SHA the approval names. In initial delivery it is a repository document on the default branch, held to the dossier's visibility and reference rules (Recommendation), identified like the dossier by repository, path and git blob SHA, and that blob SHA is its immutable revision identifier; it binds the approved dossier's repository, path and blob SHA and, for an infrastructure target, its current generation, with a freshness bound the dossier declares. The removal authorization names that exact assessment revision. Refreshing an assessment creates a new revision, preserves the earlier assessment, and requires the removal authorization to bind the refreshed revision before execution. The assessment adds no new state machine. Each removal criterion is passed, failed, or unknown with source/time/coverage. Missing or stale evidence is unknown (ADR-079, `docs/adr/079-product-and-program-management-agent.md:88`). Changed consumer state or stale evidence requires refresh.

Every required removal criterion must pass under the approved dossier revision before removal is eligible. Failed or unknown required criteria hold removal. An approver the D5 predicate admits may approve a revised retirement commitment that accepts specified remaining exposure, followed by a fresh assessment; historical failed and unknown results retain their original dispositions. The change is a risk decision, not a fabricated proof of no consumers. Identity, authorization, and data-disposition requirements remain mandatory.

### D5. Authorize and execute removal as an exact intent

Code removal follows the normal reviewed merge and release path, with the dossier identifying the first merge, release, or configuration activation that makes the capability unavailable. Before that boundary is crossed, the authorization for that boundary names the current dossier revision, D4 assessment revision and explicit removal decision; ordinary code approval alone does not establish retirement readiness. No product check enforces this until the removal-dispatch ADR below. Until the removal-dispatch ADR the change that crosses the boundary uses an approval-gated review, and the operator merges it only after the authorization is recorded; a delegated merge does not satisfy this, and a check-only review gate is not eligible, because it queues a forge auto-merge (`docs/spec/workflow-v2.md:1231`).

Infrastructure shutdown/removal remains an operator-controlled external pipeline action, with a pre-execution authorization naming the dossier revision, D4 assessment revision, exact resources, current versions/generation, removal configuration, recovery procedure, and required verification.

D2's commitment, D4's risk acceptance, this section's removal and D6's deletion each require a forge-verified non-agent identity satisfying the repository's declared approvals predicate for that action (one predicate, or one per action, at the repository's choice), in the plan-gate `approvals` shape with a non-empty `members` list and at least `not: [agent]`; no `operator_agent` delegation condition (`docs/adr/040-operator-agent-role-contract.md:52`) or standing order satisfies any of them. In initial delivery each authorization is a forge act by the admitted identity naming the SHAs (D2), and no stage gate carries one until the removal-dispatch ADR, which declares `amends: ["ADR-040"]` if it puts one on a gate. The predicate declares no `author` leg, because the change-author resolver reads a run's audit rows and has no answer here (`backend/internal/server/quorum.go:176-190`).

Guarded product execution remains unavailable until a separate ADR (the removal-dispatch ADR) specifies how the dossier and readiness assessment bind to the promotion contract (#4898), which candidate, brief and qualification requirements remain applicable, who adjudicates consumer evidence, and how the approved intent reaches the external executor and its authenticated outcome. That ADR changes the `deployment` artifact (`docs/spec/workflow-v2.md:978`) and the deploy gate's pre-execution intent, so it must declare `amends: ["ADR-038"]` and classify itself as additive or a new major. If the promotion contract (#4898) is ratified, the removal-dispatch ADR extends the `deployment` artifact as that contract leaves it. Absence is never represented by an empty digest or a forged successful deployment artifact. Initial delivery uses operator-maintained records and separately authorized external operations; it does not certify retirement through the existing deployment artifact.

No arbitrary shell execution is added to Fishhawk. If a provider cannot enforce the intent or report the required evidence, the action remains a separately recorded human operation, not an automatically certified removal.

### D6. Data disposition is separately authorized

Shutdown approval never implies permission to purge databases, backups, audit history, object storage, credentials, or published artifacts. The dossier inventories retained data, export/migration needs, responsible owner, applicable retention/hold instructions supplied by the operator, access policy, and intended deletion method/window; it names credentials and stores by opaque reference only, never a secret value or a guessable hash of one. This ADR does not decide legal retention periods.

Destructive deletion requires a separate authorization naming the dossier blob SHA (which carries the data scope), the immutable revision identifier of a retention/hold assessment recorded as D4 records an assessment, the deletion method, the immutable executor configuration, and a validity window. It is given by a forge-verified non-agent identity satisfying the repository's declared approvals predicate; no `operator_agent` delegation condition or standing order satisfies it (D5). In initial delivery, deletion is a separately authorized external operation that revalidates the holds and the resource identity immediately before execution. Revocation or changed scope or holds blocks undispatched work; in-flight work is reconciled and recorded. Under the removal-dispatch ADR, before each independently dispatchable destructive step, the external executor must validate that authorization, current resource identity, and applicable holds under the adopted fencing contract. Product-dispatched deletion remains unavailable until that contract is implemented and demonstrated. Expiry of a time window is not authorization. If safe retry cannot be established, a lost acknowledgement is reconciled rather than replayed. A partial deletion records what is known and what cannot be recovered; the system must not promise rollback for irreversible destruction.

Retiring a service does not delete Fishhawk's audit entries, which are append-only (`docs/ARCHITECTURE.md:423`). No record decides a retention period (`docs/MVP_SPEC.md:431`, `docs/ARCHITECTURE.md:397`), and this ADR does not. The operator's retention instructions govern the retired service's own evidence. Removing customer infrastructure cannot silently delete the evidence needed to establish what was approved and done.

### D7. Verify the end state and close each obligation honestly

Verification checks the intended disabled/absent state through an authoritative platform view, replacement behavior for remaining consumers, removed routes/schedules/integrations, revoked credentials where specified, and continuing retention/export obligations. A failed network request is not proof that a resource was deleted: it may reflect missing access or an outage. In initial delivery the operator records the observation and its source; an absent or unauthorized observation is unknown and holds closure. An authenticated evidence channel belongs to the removal-dispatch ADR.

Removal, archive, deletion, and consumer-migration completion are distinct facts. The operator confirms completion against the dossier; unfinished retention or deletion work stays linked and owned rather than being hidden by closing the code-removal issue. A later unexpected consumer creates recovery or follow-up work with new authorization; it does not erase the retirement decision. Where the promotion contract (#4898) is in force, deregistration of an environment or adopted suite follows this verification, through the adoption path those declarations require.

Retain references to the final evidence, responsible owner, and any support/restore window. The driver may record lessons under ADR-125 where a decision-bearing audit entry on the episode's run, or on a member run of its campaign, can be cited (`docs/adr/125-captains-log.md:65`), and files relevant defects through existing intake. Ongoing monitoring for unexpected consumers remains external, with declared alerts where useful.

### D8. Keep the mechanism portable and bounded

Repository configuration supplies resources, consumer sources, policy pointers, pipeline targets, and approval predicates under ADR-110. Once the removal-dispatch ADR gives the product a surface that reads these declarations, a missing approvals predicate or dossier home refuses with a named reason and a missing consumer source yields unknown (`docs/adr/110-repository-agnostic-product-features.md:77-80`); that ADR names the ADR-110 rule-1 surface holding them. Until then the predicate and dossier home are declared on an ADR-110 rule-1 surface (one of the three `.fishhawk/` files, or a document one of them references by path), read from the default branch, and the operator's record names that path and blob SHA; where either is missing the operator refuses and records the refusal; a dossier that declares no freshness bound makes every assessment stale and therefore unknown. A service, library API, schema major, and feature flag need different evidence; no Fishhawk-specific path, environment name, or mandatory cloud provider is embedded in the product.

The first walk should retire a concrete low-risk capability. Do not implement a universal deletion engine or multi-repository transaction. Cross-repository consumers can supply evidence, but their repositories retain authority over their own migrations and deployments.

## Decision

**Accepted 2026-11-04** by the Captain (1 of 1; manual ratification path, the ADR-113 D2 verb is not yet built; approver and hashes in the ratification comment). **Option B**, i.e. D1–D8 as stated in the Recommendation, decided now as a procedure for operator-maintained records and separately authorized external operations. Product-dispatched removal or deletion is not decided here and needs a separate ADR. This record supersedes and amends nothing, permits no concrete retirement, and adds no stage type, tracker or destructive authority.

## Consequences

Retirement becomes an accountable sequence of preparation, exact removal, verified outcome, and separate data disposition. Existing workflows carry most execution; the new work is the dossier/approval contract and, where automated certification is needed, the removal-dispatch ADR (D5). Consumer completeness and external telemetry remain imperfect, so uncertainty must remain visible. A process holding an approver's forge credentials can approve a removal or disposition as that approver; the control is procedural (ADR-113).

No new stage type, permanent project run, tracker, or autonomous destruction authority is introduced. Ordinary preparation can proceed under existing decisions once separately scoped. Fishhawk-dispatched production removal waits for the promotion/recovery contract, the adopted removal-dispatch ADR, and demonstrated provider enforcement and evidence support. Independently authorized human operations remain external and are recorded with their evidence limitations; they do not acquire automated certification. Existing compatibility decisions continue to govern; no retention decision is recorded.

- **Removal-dispatch ADR.** Product-dispatched removal or deletion requires a separate ADR (D5). That decision is not made here.
- **Enablement condition on that ADR.** Before enabling product-dispatched removal or deletion, demonstrate a low-risk end-to-end walk from immutable dossier and assessment through distinct authorizations under the D5 predicate, external execution, authoritative verification, and linked residual obligations. Exercise stale or revoked authorization, failed authorization persistence, lost acknowledgement, restart, and cross-use of removal and deletion approvals. Prove refusal at the external executor as well as in Fishhawk. A non-Fishhawk fixture must exercise changed and missing declarations through the production resolver and consumer.
- **Dependency.** Guarded production execution depends on the promotion/recovery contract (#4898) being ratified and implemented.
- **No concrete retirement.** This ADR does not authorize any concrete retirement.
- **Residuals.** Outside a stage gate the dossier's author is not excluded from authorizing it; the controls are the predicate's count and members.
- **Record metadata.** amends: []; supersedes: []; applies_to: []
- **Related:** Companions, each an unratified proposal that citing here does not ratify: #4897 (distinct dev, stage, and production delivery environments), #4898 (immutable release candidates and environment promotion), #4899 (deterministic production smoke verification), #4901 (ADR-079 amendment: experiments and lifecycle evidence).

### Validation

- A dossier omits a required element, or a deprecation is read as removal: the retirement commitment is not established (D1).
- Criteria or data handling change after approval: the change requires a new decision and is not treated as the same one (D2).
- An agent-drafted notice is offered as communication evidence: it is recorded as a draft, not as a sent notice (D3).
- Telemetry is empty because collection stopped: removal readiness remains unknown.
- An infrequent/offline consumer appears after the observation window: its compatibility promise is evaluated, not assumed expired.
- The dossier or target generation changes after approval: the old authorization, which names the earlier blob SHA and generation, does not cover the new scope. Until the removal-dispatch ADR, this is an operator check of the recorded authorization, not a product refusal.
- A service is stopped but its database has a retention obligation: deletion is refused without separate valid authority.
- Once the removal-dispatch ADR and the promotion contract are ratified and implemented: duplicate removal callbacks, missing acknowledgement, partial deletion, revoked provider access, and external interference retain accurate outcomes without unsafe retries.
- A provider query is unauthorized: it cannot be recorded as proof that a resource is absent.
- Code removal is complete while archive/retention work remains: the outstanding obligation stays visible and owned.
- A non-Fishhawk fixture retires an API version rather than an infrastructure service, exercising declarations through the same assessment and approval contract.
- An approval appears only as a line in the dossier or the operator's record: it is not an approval, and removal stays held.
- A cited approval comment or review is edited on the forge after the record cited it: the edited act is a new act to evaluate, and the earlier citation does not cover it (D2).
- An assessment is stored inside the dossier, or a removal authorization names an assessment that has since been refreshed: the authorization does not cover the refreshed revision.
- The change that crosses the removal boundary is configured with a check-only review gate or a delegated merge: neither satisfies D5; the change uses an approval-gated review, and the operator merges it only after the authorization is recorded.
- The approvals predicate declaration differs from the blob SHA recorded with the approval: the approval is re-evaluated against the current declaration.
