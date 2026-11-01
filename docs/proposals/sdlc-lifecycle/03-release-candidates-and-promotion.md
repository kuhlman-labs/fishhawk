# Release candidates bind qualification, approval, deployment, and recovery

**New ADR draft v1 — proposed 2026-10-31; number unallocated.** Prepared at the Captain's request. Proposed relations on ratification: `amends: [ADR-038, ADR-049, ADR-051, ADR-079]`; supersedes none. Companion: [production smoke verification](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr-production-smoke/docs/proposals/sdlc-lifecycle/04-production-smoke-verification.md). This proposal records no ratification or deployment authorization.

## Context

The existing decisions cover delegated deploys (ADR-038), independent acceptance (ADR-049), release evidence and publication (ADR-051), and release readiness (ADR-079). They do not jointly guarantee that the artifact qualified in stage is the one approved and then observed in production.

The present release workflow dispatches `deploy.yml` at `main`. The deploy reconciler can observe the pipeline's `head_sha`, while a pipeline may actually deploy another image. The acceptance identity probe also permits an unverifiable target to proceed with a warning. Those facts cannot support a claim that a production artifact was qualified and approved merely because the pipeline is green.

A release also spans many bounded runs. It needs attributable links among the ratified brief, merged work, built artifacts, qualification, approval, actual deployment, and later observations without creating a second tracker or a project-wide execution state machine.

## Options

- **A. Keep ref-based deployment and reconstruct the release afterward.** Minimal work; mutable refs, rebuilds, and configuration drift make evidence and approval ambiguous.
- **B. Immutable candidate and deployment-intent contracts over existing runs and pipelines (recommended).** Fishhawk governs identity, evidence, and decisions; external systems retain build, rollout, credentials, and monitoring.
- **C. Own the build and deployment platform.** Rejected: crosses ADR-038 and MVP_SPEC §9's boundary and duplicates customer infrastructure.

## Recommendation

Adopt option B. The following are product contracts, not an assertion that the named records or wire fields exist today. Schema, persistence, endpoints, and callback authentication must be specified in implementation plans before they are exposed.

### D1. A candidate is an immutable manifest

A candidate identifies its repository, ratified release-brief revision, included change/evidence references, source commit(s), and exact component artifacts by content digest. Include build provenance and signature-verification references, deployment package/chart digest, migration-set digest, compatibility assumptions, and declared feature/configuration expectations. A source SHA, tag, semver string, or successful pipeline alone is not an artifact identity.

The manifest has a canonical encoding and content hash. Any material field change creates a new revision/hash; published evidence is never edited into agreement with a newer candidate. A rebuild is a different candidate even when the source commit is unchanged. Human-readable version labels may point to candidates but never replace their hashes in authorization.

Initial scope is components within one repository. External dependencies are referenced by pinned identities and compatibility evidence; this introduces no cross-repository campaign or approval authority and does not depend on proposed ADR-130.

### D2. Environment registration is an authority boundary

An operator-controlled, versioned registration binds a stable target identity to the owning repository, environment class (`development`, `qualification`, or `production`), allowed delegate/pipeline identity, target identity verifier, configuration-reference policy, and external credential binding names. Customer display names are independent of class. Unknown targets/classes refuse guarded deployment; `prod`, `production`, and hostname aliases cannot lower the class.

Registration and changes are Tier I under ADR-121, adopted by a forge-verified non-agent identity with the repository's configured authority (ADR-113/123). A candidate cannot register or reclassify the environment judging it. Secret values stay external; include only access-controlled opaque version references where behavior depends on them. Never store a digest of a low-entropy secret as a substitute for its value.

The effective registration and policy are snapshotted into each deployment intent and checked for current validity again at dispatch. Revocation holds undispatched work. In-flight revocation does not pretend to undo a side effect; it holds subsequent promotion, records the uncertainty, and routes recovery to the operator.

### D3. Qualification establishes release evidence, not deployment authority

A qualification attempt binds the candidate hash, release-criteria revision, environment identity and generation, effective non-secret configuration digest, check-suite revision, and evidence cutoff. It records each required criterion as passed, failed, or unknown, with bounded source references and limitations. Zero executed required checks, missing results, unavailable identity, or stale evidence cannot become a pass.

Per-change CI/preview evidence may support a release criterion where applicable, but does not automatically establish integration of the assembled candidate. Release qualification covers the applicable installation, upgrade, compatibility, migration, performance, accessibility, security, and recovery criteria in the approved brief. Scope is declared per release; this is not a fixed universal suite.

Each attempt has a controlled target generation. A concurrent replacement or relevant configuration change invalidates its observations. Different candidate or criterion revisions require a new assessment. Evidence reuse is explicit and conservative: unchanged content, test inputs, environment contract, applicable dependencies, and freshness must be demonstrated by the registered criterion's reuse rule; absent such a rule, rerun. This is separate from ADR-096/124's source-tree verify-verdict transfer.

Initially, production promotion requires all designated release-blocking criteria to pass. Nonblocking findings and unknowns remain visible. Revising which criteria block is a new human-ratified brief/policy revision and triggers reassessment; it never relabels a historical failure as success. Identity, authorization, and evidence-authenticity controls are not waivable criteria.

### D4. Readiness, approval, execution, and verification are distinct claims

ADR-079 readiness consumes the exact brief, candidate, and qualification revisions and recommends ship, narrow, or wait. The recommendation grants no release authority. ADR-051 publication and version selection remain separate actions.

The production approval statement binds at least:

- candidate manifest hash and readiness/qualification references;
- target registration revision and environment identity;
- delegate identity and immutable executable pipeline/configuration revision;
- effective target configuration, migration plan, and approved secret-version references where required;
- expected currently deployed predecessor and target generation;
- rollback/roll-forward procedure revision and the permitted recovery target;
- approval expiry and the adopted production verification policy revision.

A forge-verified human approval quorum satisfying the repository's count and eligibility predicates authorizes that exact intent. This draft neither reduces a configured quorum nor requires a second approver where only one is configured. Provider-required environment approvals remain independent controls. Changing a bound field, the predecessor, required evidence, or relevant authority invalidates pending authorization. Time-bound evidence and expiry use authoritative recorded times, not an agent's assertion. Re-evaluation immediately before dispatch closes stale-approval gaps.

A release surface shows qualification, authorization, rollout outcome, and production smoke outcome separately. A successful deployment stage is not synonymous with a production-verified release. Recovery creates a new observation/attempt; it does not overwrite the failed history.

### D5. Dispatch is idempotent and fenced per target

Use a durable intent/attempt identity and an atomic authorization-to-dispatch claim before any external request. Repeated calls with the same intent reconcile the existing attempt. A timeout after sending a request means dispatch status is unknown; look up by correlation before sending again. If the provider cannot prove whether it accepted the dispatch, hold for reconciliation rather than risk a duplicate side effect.

Every target-changing path participates in a per-environment generation/fencing contract: forward deploy, rollback, retry, manual workflow dispatch, and declared emergency recovery. The external pipeline validates the intent, expected predecessor, and current fence before acting. A stale worker cannot deploy after a later authorized attempt acquires the target. Application of this fence in the real platform is an enablement prerequisite, not something a backend lock alone proves.

Deployment serialization is per environment, not a repository-wide lock on development or review. It does not modify ADR-115's pre-merge parallelism or the merge queue. An external out-of-band deployment breaks the observed generation, invalidates pending qualification/approval, and holds promotion until reconciled. Break-glass recovery stays possible through the customer's authority, but is recorded honestly as external recovery, not retroactively approved.

### D6. Promote the qualified artifacts and observe actual identity

The external pipeline consumes the approved artifact digests and deployment intent. It may adapt explicitly declared environment configuration, but cannot rebuild the application or resolve an unbound mutable tag for production. Signature/provenance verification binds the artifacts to the permitted builder and source under the adopted policy. The pipeline remains the owner of deployment mechanics and production credentials.

The resulting observation names the intent/attempt, target identity/generation, actual artifact digests, applied configuration and migration revisions, execution identity, provider run/attempt, timestamps, outcome, and recovery reference. Correlate by exact IDs, not newest workflow run or branch name.

Authenticate reports through the adopted provider/workload identity and verify its repository, workflow, immutable executable revision, attempt, and target authorization. Reject cross-target/cross-repository substitutions and replay; an identical duplicate is idempotent, a conflicting duplicate holds the result. The target's observed identity comes from the registered platform verifier, not a model's narrative or the application's untrusted self-report alone. Inability to establish identity yields unknown and blocks a verified-release claim.

Fishhawk need not fetch customer source or runtime. A customer-side verifier observes the platform and ships bounded authenticated evidence. This authenticates who observed what under which adopted checker; it does not prove that a compromised authorized pipeline told the truth. Protecting that pipeline and checker is part of the threat model.

### D7. Recovery names the exact deployment and preserves data semantics

A recovery request targets a deployment intent/attempt and environment, not implicitly the run's first deploy stage. The current run-scoped rollback selection must be replaced or supplemented with unambiguous selection before this contract is enabled. The request records the intended recovery artifact, expected current generation, recovery procedure, data-compatibility preconditions, and authorizing decision.

The pipeline may perform a previously approved automatic rollout recovery such as Helm atomic rollback within that intent. Otherwise the operator explicitly chooses rollback or roll-forward. A failed migration or partially applied change never triggers an unbounded blind redeploy. Irreversible data changes require a forward-recovery plan and separately authorized destructive actions.

Rollback initiation, pipeline completion, observed resulting identity, and recovery verification are separate evidence. Failed or unknown recovery cannot produce a successful `rolled_back` claim. A conflicting external recovery requires reconciliation, not another automatic attempt. Restore drills and retained recovery artifacts are required where the release promises recoverability.

### D8. Release publication does not rebuild or rewrite history

Amend ADR-051's publication contract to link evidence-derived notes and published versions to the immutable candidate. Human-owned version/tag decisions remain. The tag/publication pipeline must promote or package the already qualified bytes where feasible without changing their identity. If packaging/signing changes the deployable bytes, that result is a new candidate requiring qualification before production.

This repository's currently tag-driven build path must be reconciled explicitly: either build the candidate from the release tag before qualification, or adopt a pipeline that publishes existing candidate artifacts. Never claim build-on-tag and rebuild-on-promotion preserve identical artifacts without proof.

Published GitHub Releases remain immutable under ADR-051's existing amendment. Finalize notes/assets before publication; later corrections use a separately attributed correction mechanism supported by the provider. Publishing a release does not dispatch production implicitly. A production deployment does not imply that release notes were published.

### D9. Scope, compatibility, and incremental enablement

The product contract is repository- and provider-neutral (ADR-110). A first provider implementation can be explicitly scoped to GitHub Actions plus one external verifier. Unsupported delegate capabilities refuse guarded promotion by name; the system cannot silently fall back to ref-only dispatch.

Enable this contract explicitly per workflow/target. Existing deploy workflows keep their documented behavior and are labelled legacy/unqualified rather than retroactively certified. Existing acceptance shapes remain valid. New artifact/schema/API fields follow the additive-versus-major rules; no sixth stage type or new acceptance executor is introduced. For this repository, adopting production promotion requires migrating every authorized production entry point, not leaving a legacy bypass beside the new gate.

## Decision

Pending Captain review and ratification under ADR-113. No option has been adopted. Proposed amendments: ADR-038 for exact-intent dispatch/recovery; ADR-049 for candidate-bound release qualification while preserving preview acceptance; ADR-051 for immutable candidate publication; ADR-079 for readiness's candidate/qualification binding. Their other decisions remain governing.

## Consequences

The release has one navigable evidence chain without making Fishhawk a build/deploy platform. The costs are real product work: immutable manifest and intent storage, environment registration, approval binding, evidence authenticity/freshness, dispatch fencing/reconciliation, exact recovery selection, and clear operator state. Human approval is meaningful only if the delegated pipeline enforces the binding too.

Implementation must specify the wire protocols, schemas, authentication/rotation, retention, supported provider capabilities, secret-reference treatment, and transition rules before building each slice. Record new audit categories and cross-module contracts in their canonical registries. Required data that cannot be safely exposed is represented by an authorized opaque reference; source ACLs govern every linked view. Missing private evidence cannot be counted as passing in an unauthorized partial view.

No new alpha blocker or production authority is created by ratification. Installations and live production walks require separately approved configuration after validation. The three-environment profile is adopted separately through the ADR-073 amendment.

### Validation and delivery gates

1. A synthetic non-Fishhawk repository builds two components and qualifies a candidate against a declared environment. Different declarations change behavior through production resolvers.
2. The same source is rebuilt to different bytes; a mutable tag moves; the pipeline tries another digest: all invalidate or refuse the approved deployment.
3. Criteria, target configuration, checker revision, credential reference, registration, or predecessor changes between approval and dispatch: authorization is held, not silently transferred.
4. A callback is duplicated, conflicts, is replayed from another attempt, or names another repository/target: identity checks reject or hold appropriately.
5. A dispatch acknowledgement is lost or the backend restarts after sending: exactly the known external attempt is reconciled; no blind duplicate dispatch.
6. Two deployments, a rollback, and a manual pipeline compete for the same target: only the valid fence can act; unrelated development remains parallel.
7. Pipeline success with a wrong/unverifiable artifact cannot produce a qualified deployment. Missing or zero required checks cannot produce qualification success.
8. A failed migration, unavailable rollback handle, failed rollback, or forward-incompatible database produces an explicit recovery state, never a fabricated successful rollback.
9. A tag-driven release publishes immutable assets and evidence without rebuilding the promoted candidate or attempting an unsupported published-Release rewrite.
10. A complete operator walk demonstrates intent → qualification → approval → dispatch → observed outcome → recovery, including restart and stale-evidence cases, before production enablement.
