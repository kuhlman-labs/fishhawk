# Retirement separates consumer migration, removal, and data disposition

**New ADR draft v1 — proposed 2026-10-31; number unallocated.** No supersession or automatic amendment of a supported-version decision is proposed. Depends on the [promotion/recovery contract](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr-release-promotion/docs/proposals/sdlc-lifecycle/03-release-candidates-and-promotion.md) for guarded production execution. Preparation can use existing workflows. This draft does not authorize any concrete retirement.

## Context

The delivery lifecycle needs a governed exit as well as a release path. Removing an API, component, service, feature flag, environment, or data store can affect consumers after the code is merged. Zero recorded use does not establish zero consumers, and shutting down a service does not authorize deleting its retained data.

Existing decisions handle particular removals and compatibility rules: ADR-073 retires the ECS path; ADR-046 preserves workflow majors until a separate sunset decision. Existing campaigns and change/deploy workflows can carry the work, but there is no common retirement evidence and authorization contract.

## Options

- **A. Treat retirement as an ordinary delete-code issue.** Simple, but cannot establish consumer readiness, external removal, or data authority.
- **B. A versioned retirement dossier carried by ordinary work, with separately gated removal and data disposition (recommended).** Reuses existing tracker, campaign, approval, deployment, and audit surfaces.
- **C. A new retirement stage type and long-running lifecycle engine.** Deferred: retirement often spans several releases; a perpetual run or new workflow type adds little authority or evidence.

## Recommendation

Adopt option B. The initial dossier is a reviewed, versioned repository document referenced by work items and approval statements; it is not a new backend state machine. Proposed field names below describe its required content. A future typed API/artifact would require its own implementation design and schema review.

### D1. Define exactly what is being retired

The dossier names the capability/resource identity, owner, supported versions and consumers, reason, replacement, desired end state, known dependencies, evidence sources, and proposed dates/windows. It distinguishes deprecation (supported but discouraged), disabled operation, removal, archive/retention, and deletion. A component replacement is not automatically a retirement of its stored data or published historical artifacts.

Consumer evidence includes measured usage, declared dependents, offline or infrequent clients, external integrations, and inaccessible/unknown populations. Measurement windows and blind spots are stated. "No observed use" is only evidence about the observed population and period. A deadline does not automatically authorize removal.

### D2. Human approval establishes the retirement commitment

The Captain approves the exact dossier revision, evidence basis, affected audiences, migration path, notice requirements, removal criteria, risk owner, and recovery/abandonment conditions. Subsequent changes to scope, target, criteria, or data handling require a new decision. Existing supported-version or compatibility promises remain governing: retiring a workflow major under ADR-046 still needs the specific sunset ADR it requires.

This generic ADR supplies procedure; it does not sunset any version. A retirement that changes architectural or public compatibility commitments is assessed against the ordinary ADR criteria before execution.

### D3. Preparation is a campaign of ordinary changes

Use ADR-052 refinement and ADR-047 campaigns to deliver replacement behavior, adapters, telemetry, migration tools, documentation, and consumer migration. Work items reference the dossier revision and acceptance criteria. Existing feature/routine/human-led workflows apply according to scope; there is no path around protected instruction, CI, policy, or legal files.

Notices and customer communications remain operator-owned external actions. Record the communication evidence and intended audience; an agent-authored draft is not a sent notice or a proof that all consumers received it. The campaign may span releases without leaving one run parked for months.

### D4. Assess readiness immediately before removal

Produce a dossier-bound assessment of migrated consumers, remaining usage, notice evidence, replacements, dependency state, retention obligations, recovery artifacts, and operator ownership. Each removal criterion is passed, failed, or unknown with source/time/coverage. Changed consumer state or stale evidence requires refresh.

Required unknowns hold removal unless the authorized human explicitly changes the retirement commitment and accepts the named remaining exposure. The historical evidence stays unknown; the change is a risk decision, not a fabricated proof of no consumers. Identity, authorization, and data-disposition requirements cannot be waived into existence.

### D5. Authorize and execute removal as an exact intent

Code removal follows the normal reviewed merge and release path. Infrastructure shutdown/removal remains an operator-controlled external pipeline action, with a pre-execution human gate naming the dossier revision, exact resources, current versions/generation, removal configuration, recovery procedure, and required verification.

The promotion contract's immutable intent, authenticated executor, per-environment fencing, idempotency, and recovery selection apply. For operations that remove rather than deploy an artifact, the intent explicitly identifies the previous artifact/resources and desired absent/disabled state. This requires a documented extension of the deployment intent/outcome schema before it can be consumed by the product; absence is not represented by an empty digest or a forged successful deployment artifact.

No arbitrary shell execution is added to Fishhawk. The existing delegate mechanism triggers the registered external action under its own credentials. If a provider cannot enforce the intent or report the required evidence, the action remains a separately recorded human operation, not an automatically certified removal.

### D6. Data disposition is separately authorized

Shutdown approval never implies permission to purge databases, backups, audit history, object storage, credentials, or published artifacts. The dossier inventories retained data, export/migration needs, responsible owner, applicable retention/hold instructions supplied by the operator, access policy, and intended deletion method/window. This ADR does not decide legal retention periods.

Destructive deletion requires its own human authorization bound to the exact data/resource scope and the current retention/hold assessment. Expiry of a time window is not authorization. If safe retry cannot be established, a lost acknowledgement is reconciled rather than replayed. A partial deletion records what is known and what cannot be recovered; the system must not promise rollback for irreversible destruction.

Audit evidence is retained under its governing policy even when the service is retired. Removing customer infrastructure cannot silently delete the evidence needed to establish what was approved and done.

### D7. Verify the end state and close each obligation honestly

Verification checks the intended disabled/absent state through an authoritative platform view, replacement behavior for remaining consumers, removed routes/schedules/integrations, revoked credentials where specified, and continuing retention/export obligations. A failed network request is not proof that a resource was deleted: it may reflect missing access or an outage.

Removal, archive, deletion, and consumer-migration completion are distinct facts. The operator confirms completion against the dossier; unfinished retention or deletion work stays linked and owned rather than being hidden by closing the code-removal issue. A later unexpected consumer creates recovery or follow-up work with new authorization; it does not erase the retirement decision.

Retain references to the final evidence, responsible owner, and any support/restore window. Feed lessons into ADR-125 and relevant defects into existing intake. Ongoing monitoring for unexpected consumers remains external, with declared alerts where useful.

### D8. Keep the mechanism portable and bounded

Repository configuration supplies resources, consumer sources, policy pointers, pipeline targets, and approval predicates under ADR-110. A service, library API, schema major, and feature flag need different evidence; no Fishhawk-specific path, environment name, or mandatory cloud provider is embedded in the product.

The first walk should retire a concrete low-risk capability. Do not implement a universal deletion engine or multi-repository transaction. Cross-repository consumers can supply evidence, but their repositories retain authority over their own migrations and deployments.

## Decision

Pending Captain review and ratification under ADR-113. No option has been adopted. Existing compatibility and retention decisions continue to govern. This proposal permits no retirement, destructive action, communication, or deployment by itself.

## Consequences

Retirement becomes an accountable sequence of preparation, exact removal, verified outcome, and separate data disposition. Existing workflows carry most execution; the new work is the dossier/approval contract and the removal-intent extension where automated certification is needed. Consumer completeness and external telemetry remain imperfect, so uncertainty must remain visible.

No new stage type, permanent project run, tracker, or autonomous destruction authority is introduced. Ordinary preparation can proceed under existing decisions once separately scoped. Production execution waits for the promotion/recovery contract and the provider's removal evidence support.

### Validation

- Telemetry is empty because collection stopped: removal readiness remains unknown.
- An infrequent/offline consumer appears after the observation window: its compatibility promise is evaluated, not assumed expired.
- The dossier or target generation changes after approval: the old authorization cannot remove the new scope.
- A service is stopped but its database has a retention obligation: deletion is refused without separate valid authority.
- Duplicate removal callbacks, missing acknowledgement, partial deletion, revoked provider access, and external interference retain accurate outcomes without unsafe retries.
- A provider query is unauthorized: it cannot be recorded as proof that a resource is absent.
- Code removal is complete while archive/retention work remains: the outstanding obligation stays visible and owned.
- A non-Fishhawk fixture retires an API version rather than an infrastructure service, exercising declarations through the same assessment and approval contract.
