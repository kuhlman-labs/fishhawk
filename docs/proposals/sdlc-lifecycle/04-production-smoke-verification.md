# Production verification uses bounded pipeline checks and authenticated evidence

**New ADR draft v1 — proposed 2026-10-31; number unallocated.** Proposed relation on ratification: `amends: [ADR-053]`; supersedes none. Depends on the [release candidate and promotion contract](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr-release-promotion/docs/proposals/sdlc-lifecycle/03-release-candidates-and-promotion.md) being ratified and implemented before production enablement. No production access is authorized by this draft.

## Context

ADR-053 permits staging-only post-deploy acceptance and requires a separate security re-review before production-facing acceptance agents are allowed. ADR-050 assumes an acceptance agent may be prompt-injected. ADR-086 adds scoped credentials and target provisioning without making arbitrary production exploration safe. ADR-103's adversarial probes explicitly require disposable non-production targets.

The lifecycle needs a narrower immediate production question: did the approved deployment leave the intended service functioning? Pre-approved deterministic smoke checks can answer that without an agent choosing actions against real users and data. Detailed requirement acceptance stays in preview and stage; ongoing telemetry remains the monitoring system's responsibility.

## Options

- **A. Treat a successful deployment pipeline as verification.** Insufficient: a rollout can complete while critical behavior fails.
- **B. Deterministic smoke checks executed by the delegated pipeline, with authenticated evidence and a governed disposition (recommended).** Adds bounded operational assurance without an agent in the production data path.
- **C. Permit an acceptance agent to explore production using scoped credentials.** Deferred. This needs its own threat model, containment proof, and explicit amendment of ADR-050/053. A host allow-list alone is insufficient.
- **D. Build continuous monitoring/on-call into Fishhawk.** Rejected under ADR-053 and MVP_SPEC §9. External monitors detect; Fishhawk consumes bounded evidence and alerts.

## Recommendation

Adopt option B as a distinct production verification mode within delegated deployment evidence. Do not add a stage type, shell executor, or deterministic executor branch to `acceptance`.

### D1. Checks run outside Fishhawk's agent execution path

The operator-owned deployment pipeline runs a versioned smoke suite after the rollout establishes an observed target identity. Fishhawk receives and evaluates its bounded structured results through the promotion contract's authenticated evidence channel. Fishhawkd neither executes the checks nor directly accesses the production runtime. The agent receives no production credential, response stream, or ability to choose additional requests.

The deploy execution outcome and smoke outcome are separate records/views. The pipeline may finish while verification evidence is missing; that is a deployed-but-unverified release, never an implied pass. Implement the release-completion interlock before enablement: a workflow opting into required production smoke cannot report a verified release or advance a dependent promotion while those checks are failed, missing, or unknown. Do not infer eligibility from the deploy stage's `succeeded` state alone.

Existing `acceptance` stages, including deployment-consuming ones, retain their present non-production boundary. This ADR admits production evidence from approved external deterministic checks; it does not remove the staging-only validation guard or permit a production hostname under a staging alias. The generic target classification comes from the promotion contract, not a string comparison.

### D2. A human adopts the check policy before execution

The registered suite identifies the fixed checks, immutable executable/configuration revision, allowed target resources and operations, required results, thresholds, observation window, freshness, request/concurrency limits, timeout, retry bound, and response/evidence limits. These judge inputs are Tier I under ADR-121. A candidate cannot weaken the suite evaluating its own deployment.

Approving a deployment adopts that exact suite revision as part of the intent. A threshold change produces a new policy revision and assessment, not a retroactive pass. Existing in-pipeline automatic rollout recovery is separately identified; the smoke checker itself acquires no recovery authority.

### D3. First delivery is non-mutating and low-volume

Initially permit read-only checks whose handlers have been reviewed for side effects: target/build identity, health/readiness, authentication with a dedicated synthetic account, and a bounded read of synthetic fixture data. HTTP method alone does not establish safety; a GET can mutate or expose sensitive data. The suite must specify the actual operation and data boundary.

Exclude broad customer-data scans, real payments or notifications, load/stress tests, fuzzing, and adversarial probes. For a product whose essential smoke needs a synthetic write transaction, report that coverage gap and use stage evidence; do not silently widen the first production policy. A later revision must define isolated test-tenant writes, cleanup/compensation, idempotency, and external-effect containment before such a check is enabled.

Limits are enforced by the external execution environment and target permissions, not just stated in a prompt. The check identity cannot deploy, administer the service, read arbitrary customer records, or cross the registered target. Tokens are short-lived where the provider supports them, otherwise scoped and rotated through the customer's secret owner. Fishhawk stores binding/version references, never production secret values.

### D4. Separate the rollout identity from the checker identity

The deployment pipeline may need powerful rollout credentials. The smoke process runs in a separate isolated job/workload with only its check credentials; it must not inherit the rollout job's environment, workspace secret files, service-account permissions, credential helpers, or debug dumps. Immutable check code and the adopted configuration execute through the registered provider identity.

Direct and indirect network access is restricted to declared target and evidence destinations under the external runner's enforced policy. Refuse enablement where the required isolation cannot be demonstrated. This extends neither ADR-050's agent fallback nor the current Linux env-only posture into a production permission grant.

### D5. Evidence binds what ran to what was deployed

Each result binds the deployment intent/attempt, candidate hash, environment identity and generation, observed component identities, suite revision, check ID, start/end time, outcome, and bounded evidence references. The promotion contract's authentication, callback replay protection, and conflict handling apply. No unbound JSON upload, model-written pass, or pipeline URL alone counts as verified evidence.

Verify the target generation/identity before and after the check window. A changed or partially unidentifiable target makes the affected result unknown. For services with several replicas or components, the suite declares the coverage and rollout-completion observation needed to establish the intended version set; one response from one healthy replica cannot silently establish full rollout completion.

Use a closed outcome vocabulary for each required check: pass, fail, unknown. Preserve attempt history through retries. A retry is bounded and records why it occurred; a later pass does not erase an earlier failure. Required absent, expired, unexecuted, contradictory, or unverified checks yield unknown/hold. A deterministic evaluator computes the aggregate result under the pinned policy; an agent cannot reclassify it.

### D6. Keep sensitive runtime data out of the model and audit payloads

Prefer statuses, durations, bounded counts, and cryptographic artifact identities over response bodies. Use an allow-list evidence schema, size limits, and customer-side redaction before upload; deny arbitrary log/HTML/request dumps. Synthetic-account identifiers and internal topology can also be sensitive and follow repository/target ACLs.

Raw troubleshooting material remains with the external operator-controlled system. A source reference grants no additional read authority. Security findings follow ADR-126's confidential record path; public issues and notifications receive only permitted summaries. If a check cannot produce safe evidence, its result is unknown until an authorized review resolves the operational decision.

### D7. Failures route to a decision, not arbitrary agent recovery

A failed or unknown required result holds verified completion and pages the configured operator through existing gate/notification mechanisms. The evidence states whether the problem is an observed regression, an unavailable check, stale identity, or an inconclusive result. Monitoring alerts can independently enter ADR-053's incident intake.

The operator chooses a bounded recheck, a separately approved rollback/roll-forward, or continued operation under an explicit recorded risk decision. Continued operation does not change a failed/unknown check to pass or label the release verified. Any temporary exception has an owner, scope, expiry, and follow-up; identity/authentication failures cannot be converted into valid verification evidence by an exception.

Recovery uses the exact-deployment selection and fencing in the promotion draft. After recovery, check the newly observed artifact/configuration under a fresh verification attempt. A successful rollback pipeline alone does not establish a recovered service. Neither a smoke failure nor an agent's diagnosis permits changing production data or code in place.

### D8. The observation window is bounded

Immediate smoke and a short, declared stabilization window can consume trusted external metrics if their source, query revision, population, baseline, and missing-data behavior are adopted in advance. Missing traffic, absent baseline, or an unavailable monitor means insufficient evidence, not zero errors. No statistical reliability claim is inferred from a few synthetic requests.

At the end of the window, ongoing health belongs to existing observability/on-call systems. ADR-079 consumes later product outcomes and ADR-053 consumes alerts. Fishhawk does not run an indefinite release stage waiting for the service's lifetime.

## Decision

Pending Captain review and ratification under ADR-113. If adopted, this amends ADR-053 by admitting production verification through the bounded external evidence path above. Its prohibition on production-facing acceptance agents remains intact. ADR-050, ADR-086, and ADR-103's agent containment/target boundaries are not widened.

## Consequences

The production release gains reproducible smoke evidence while keeping agents out of the runtime credential path. The price is a trusted external checker, authenticated ingestion, target identity verification, explicit aggregation/freshness, and operator handling for failed or unavailable checks. A compromised approved pipeline or checker remains a threat; its identity and integrity are part of the adopted supply-chain boundary, not solved by signing its output alone.

First implement and validate the protocol on a non-production target, then perform an operator-approved production activation. Existing deployments remain legacy/unverified until explicitly migrated. Production suite installation is Tier I; callback/evaluator machinery follows ADR-121 Tier M controls. No alpha milestone or autonomous production authority is added.

### Validation

- Deployment succeeds but every smoke check is missing: result is unverified and completion is held.
- Wrong candidate/replica, target alias, changed generation, or unverifiable identity cannot pass.
- Forged, replayed, cross-repository, expired, oversized, or conflicting evidence is rejected/held with a safe reason.
- The checker cannot obtain rollout credentials or read non-synthetic customer records; requests and retry bounds are enforced.
- Missing telemetry/traffic cannot appear as a favorable baseline; a failed first attempt stays visible after a retry passes.
- A candidate edits the checker or thresholds: the governing policy remains the separately adopted revision.
- Failure, unknown, authorized continued operation, rollback failure, and verified recovery remain distinguishable end to end.
- A non-Fishhawk service uses its own identity verifier, endpoints, and metrics through declarations; `/healthz` with Fishhawk's `git_sha` is not a product requirement.
