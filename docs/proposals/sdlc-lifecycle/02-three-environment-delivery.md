# Distinct dev, stage, and production environments qualify the deployed service

**New ADR draft v1 — proposed 2026-10-31; number unallocated.** Proposed relation on ratification: `amends: [ADR-073]`; supersedes none. Revises [ADR-073](../../adr/073-kubernetes-helm-single-deployment-substrate.md), tracker [#2298](https://github.com/kuhlman-labs/fishhawk/issues/2298), Decision §2 and refines §3 for this repository's deployed-service profile. This is a new amending ADR because it changes the environment policy rather than merely clarifying it. ADR-080's local alpha boundary is unchanged.

## Context

ADR-073 selected dev and prod and deferred a separate staging environment. The Captain has now requested a lifecycle with three distinct environments: dev, stage, and production. A changing integration target cannot simultaneously serve as a stable release-qualification target. Separating those purposes makes the evidence attached to a release candidate meaningful.

This is an adoption decision for Fishhawk's own deployment profile, not a requirement for all repositories using the product. It preserves Kubernetes/Helm, external pipeline ownership, the image supply chain, and the exclusion of deployment mechanics from Fishhawk itself.

## Options

- **A. Keep two environments.** Lower operating cost; release qualification competes with continuous integration or pauses it.
- **B. Three logically and operationally distinct environments (recommended).** Dev integrates continuously; stage qualifies a named candidate; production serves real users.
- **C. Require a separate cluster/account for every environment.** Stronger infrastructure separation, but unnecessarily prescribes the topology before resource and threat-model review. Production isolation must be demonstrated even where infrastructure is shared.

## Recommendation

Adopt option B through the following decisions, amending ADR-073's environment policy while preserving its other decisions.

### Three-environment deployed-service profile

**A1.1. Replace Decision §2 with these roles.**

| Environment | Deployment source | Purpose and authority |
|---|---|---|
| `dev` | Eligible builds of merged `main`; deploy each eligible head or coalesce to the newest eligible head with skipped builds recorded | Continuous integration, synthetic data, test integrations. Automatic deployment is an explicit adopted pipeline policy. |
| `stage` | An explicitly selected immutable release candidate | Stable release qualification, production-like topology and relevant configuration, representative synthetic data, controlled test integrations. A qualification attempt owns its target generation. |
| `production` | The approved candidate's same immutable artifacts | Real users and data. Explicit human deployment approval; the release's approved migration and recovery procedures govern execution. |

The stage target is not a continuously moving alias for main. It may be replaced after its qualification attempt ends; records retain which candidate and environment generation were tested. PR previews remain separate disposable targets and are not a fourth required long-lived environment.

**A1.2. Environment identity is explicit.** `dev`, `stage`, and `production` are display names backed by distinct registered target identities, classes, configuration revisions, and credential boundaries. Legacy `prod`/`staging` names must be explicitly mapped during migration; aliases must not evade production controls. Product code cannot classify safety from a hostname or string spelling alone. The companion promotion draft owns the generic environment registration contract.

**A1.3. Isolate data, credentials, and external effects.** Dev and stage have separate state and target credentials and cannot resolve production credentials. Stage uses sandbox integrations or deliberately constrained substitutes; it must not send real payments, customer notifications, or destructive external actions as a side effect of qualification. Production snapshots are not automatically copied to stage. Any proposed sanitized-data import requires an explicit handling review and cannot be assumed from this decision.

Shared clusters are an implementation option only when identities, RBAC, networking, storage, resource quotas, and failure domains meet the adopted isolation policy. Separate namespaces alone do not demonstrate that isolation. No requirement for three paid clusters is introduced.

**A1.4. Promote artifacts; declare configuration differences.** Build and sign a candidate once, qualify its immutable component digests in stage, and deploy those same digests to production. Environment-specific endpoints, capacity, credentials, and flags may differ only through declared target configuration. Stage qualification records those differences and which production claims it cannot establish. Migration and compatibility evidence binds to the deployed artifact set and configuration, not just a source commit.

The stage test configuration is frozen for the qualification attempt. A conflicting deployment or relevant configuration change invalidates the attempt; it cannot silently share an earlier pass. Credentials remain external; records contain opaque version references where needed, never values or guessable hashes of secrets.

**A1.5. Bootstrap the delivery path early.** Before committing a deployed-service release promise, demonstrate a minimal signed build, dev deployment, stage qualification, approved production deployment, and recovery with synthetic data. Infrastructure and pipeline changes are ordinary governed/operator-authored work under existing forbidden-path rules. Fishhawk onboarding does not implicitly create cloud resources, register production targets, or grant deployment authority.

The exact production activation is separately approved; this draft itself creates no environment. The bootstrap demonstration can precede broad product functionality without becoming a condition on ADR-080's local-only alpha.

**A1.6. Refine Decision §3: recovery is exercised against data evolution.** Keep Helm `--atomic --wait` as the pipeline's rollout recovery mechanism. Require stage evidence for forward migration, old/new application compatibility where promised, a failed rollout, and recovery with a forward-migrated database. Prefer staged expand/migrate/contract changes when compatibility requires them; schema contraction and irreversible deletion are separately approved steps.

Recovery objectives, backup ownership, restore evidence, and the point beyond which rollback is unsafe are declared per release/service. A successful Helm rollback does not establish database recovery or behavioral correctness. Observe and verify the resulting state. Progressive delivery controllers remain deferred; three environments do not require a canary controller.

**A1.7. Production triggers are adopted configuration, not incidental tag effects.** Replace the old automatic implication that any `backend/v*` tag deploys production. A tag may identify/publish a candidate; production dispatch must consume the exact approved candidate and target intent under the companion promotion contract. Required GitHub environment reviews remain an additional provider control. This repository's pipeline installation must prevent tag pushes or manual workflow dispatch from bypassing that contract.

**A1.8. Phase boundary.** This is the target profile for Fishhawk's deployed service. It does not require Kubernetes or three environments for the local solo-captain alpha, nor add obligations to unrelated repositories. Scheduling the deployed-service work and its live acceptance is a separate Captain decision.

## Decision

Pending Captain review and ratification under ADR-113. Decision §2 of ADR-073 continues to govern until this replacement is ratified; the live environment and pipeline configuration are not changed by this proposal.

## Consequences

Stage adds infrastructure cost and maintenance but prevents integration churn from invalidating release evidence. Environment differences remain a source of uncertainty and must be recorded rather than described as perfect parity.

After ratification: update the deployment runbooks, operator-authored pipelines, environment registrations, and relevant Helm/IaC configuration. Reconcile the actual current files before editing: the historical ADR body names paths that may since have moved or been removed. Do not enable production promotion before the companion contract is implemented.

### Validation

- Stage and dev target identities, credentials, and storage cannot be confused or cross-resolved.
- A new dev deployment cannot replace the stage candidate under qualification.
- Production deployment uses the exact stage-qualified artifact digests; a tag resolving to a different digest refuses.
- Relevant stage drift invalidates its qualification attempt; expected environment differences are visible.
- Rollback and restore drills cover a migrated database and distinguish rollout completion from behavioral recovery.
- A repository using a different environment layout still operates under its own declarations; no product default requires these three names.
