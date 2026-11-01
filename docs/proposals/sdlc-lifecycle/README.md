# Draft decisions for the project delivery lifecycle

**Draft packet v1 — 2026-10-31.** Prepared at the Captain's request. These are proposals for review, not ratified decisions, implementation authorization, or installed configuration. New ADR numbers are intentionally unallocated; the tracker allocates them when filed. Existing accepted records and their index are unchanged.

## Decision packet

| Draft | Form | Decision it contributes |
|---|---|---|
| [Product planning evidence](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr079-lifecycle-evidence/docs/proposals/sdlc-lifecycle/01-adr-079-planning-evidence-amendment.md) | Proposed in-body amendment to ADR-079 | Clarifies experiment evidence, design validation, operational criteria, and the handoffs already owned by product planning. Introduces no experiment service or new stage type. |
| [Three deployment environments](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr073-three-environments/docs/proposals/sdlc-lifecycle/02-three-environment-delivery.md) | New ADR, amending ADR-073 | Replaces the two-environment decision with distinct dev, stage, and production environments for this repository's deployed-service profile. |
| [Release candidates and promotion](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr-release-promotion/docs/proposals/sdlc-lifecycle/03-release-candidates-and-promotion.md) | New ADR, amending ADR-038, ADR-049, ADR-051, and ADR-079 | Binds qualification, readiness, approval, dispatch, and recovery to immutable candidates and explicit environment identities. |
| [Production smoke verification](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr-production-smoke/docs/proposals/sdlc-lifecycle/04-production-smoke-verification.md) | New ADR, amending ADR-053 | Adds bounded, deterministic production smoke evidence through the delegated pipeline; preserves the prohibition on production-facing acceptance agents. |
| [Retirement governance](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr-retirement-governance/docs/proposals/sdlc-lifecycle/05-retirement-governance.md) | New ADR | Governs deprecation, consumer migration, removal, and separately authorized data disposition through existing work and deployment mechanisms. |

The ADR-079 in-body amendment clarifies its existing evidence and handoff rules. The four new ADRs introduce additional decisions and would acquire their own numbers when filed; the three amending ADRs would gain reciprocal `amends` relations on ratification. No full supersession is proposed. If review expands the planning amendment into an independent experiment capability, split that addition into a new amending ADR rather than treating it as already authorized by ADR-079.

## Lifecycle coverage

| Lifecycle step | Governing decision or draft | Work remaining under that authority |
|---|---|---|
| Idea, direction, release promise | ADR-079; planning amendment | Deliver E71, with experiment observations and explicit operational criteria in the existing planning handoff. |
| Discovery and design | Planning amendment; ADR-052, ADR-084, ADR-113 | Run bounded investigations through ordinary governed work; ratify architecture choices; keep observed user evidence distinct from model inference. |
| Repository and delivery foundation | ADR-048, ADR-073; environment amendment | Onboard the repo; author infrastructure/pipelines; prove deploy, restore, and recovery. Fishhawk does not provision customer infrastructure itself. |
| Refinement, prioritization, execution | ADR-052/114, ADR-065, ADR-047 | Use existing draft approval, grooming, and campaigns. No parallel backlog or issue-selection engine. |
| Per-change verification and acceptance | ADR-027, ADR-049/050/086, ADR-084; current merge ADRs | Preserve the issue-to-plan-to-PR-to-merge loop and existing ownership of merge eligibility. |
| Continuous dev integration | Environment amendment; promotion draft | Deploy eligible main builds, associate outcomes with exact artifacts, and repair failures through ordinary work. |
| Stage release qualification | Promotion draft; ADR-079 | Qualify an assembled candidate, including installation, upgrade, compatibility, migration, recovery, and applicable non-functional criteria. |
| Release decision and publication | Promotion draft; ADR-051/079 | Bind readiness and approval to the same candidate; publish without rebuilding qualified artifacts or rewriting immutable published Releases. |
| Production deployment and smoke | Promotion and smoke drafts | Dispatch the exact approved intent, record actual deployment identity, then verify bounded operational criteria. |
| Operations, incidents, feedback | ADR-053, ADR-079, ADR-117, ADR-125/126 | Consume external alerts and observations; route recovery, fixes, confidential findings, and lessons through existing authority. |
| Deprecation and retirement | Retirement draft; ADR-046 | Migrate consumers, remove a named capability, verify removal, and disposition data separately. |

## What is deliberately reused

The repository remains the governance unit. A release brief connects many bounded runs and existing campaigns; there is no project-wide super-run, new global captain, or duplicate tracker. The stage set remains `plan`, `implement`, `review`, `deploy`, `acceptance`. The smoke draft does not add a deterministic executor to `acceptance`: its checks execute in the external deployment pipeline and report bounded evidence.

Deployment mechanics, secrets, monitoring, paging, participant recruitment, customer communications, and telemetry collection remain with their existing external owners. Fishhawk records references and decisions; an unavailable source remains unknown. None of these drafts grants autonomous production experimentation or expands agent credentials.

ADR-080's local solo-captain alpha remains the release scope. This packet defines the deployed-service lifecycle and introduces no new alpha blocker. Scheduling implementation and enabling an environment are separate decisions. Three environments are this repository's requested deployment profile, not a hard-coded requirement for every customer repository (ADR-110).

## Reading and ratification order

1. Review the planning and environment amendments independently.
2. Review the promotion contract. It supplies candidate identity, evidence binding, environment authority, and dispatch/recovery fencing used by the later drafts.
3. Review production smoke verification against that contract. Production enablement requires both implementations and their validation evidence.
4. Review retirement governance; its code-only preparation can use existing workflows, while destructive execution depends on the deployment/recovery contract.

Each proposal is independently reviewable. A reference to another draft is a conditional dependency, not a claim that it governs. No dependency on the unratified ADR-127–130 proposals is introduced. In particular, this packet does not authorize cross-repository release orchestration under proposed ADR-130.

## Implementation sequence after ratification

1. Finish the relevant E71 planning surfaces and establish the three-environment delivery foundation through operator-authored configuration and ordinary governed changes.
2. Implement candidate manifests, evidence references, environment registration, qualification/readiness views, and approval binding. Prove the path using a synthetic non-Fishhawk repository and dev/stage only.
3. Implement dispatch idempotency, per-environment fencing, observed identity, and exact-deployment recovery. Reconcile the current run-scoped rollback endpoint before enabling workflows with multiple deployment attempts or environments.
4. Implement authenticated production smoke evidence ingestion and deterministic result evaluation. Demonstrate refusal, recovery, and confidentiality cases before registering a production target.
5. Install this repository's live workflows and pipeline changes; perform the operator-led stage-to-production and recovery walk. Apply production enablement only after the evidence is reviewed.
6. Exercise retirement on a concrete low-risk capability before applying it to data-bearing production services.

These are delivery slices, not filed implementation issues. Schema/API/storage choices must be made in their plans, with the canonical schema, OpenAPI, wire parity, audit-category, documentation, and portability obligations applied. Proposed names in these drafts are logical contracts, not claims that fields or endpoints exist today.

## Review questions

- Does candidate identity bind every input that can change the deployed behavior without leaking secret values?
- Can a stale approval, replayed callback, lost dispatch acknowledgement, or competing pipeline cause an unapproved deployment?
- Can qualification or smoke report pass without checking the required criteria against the actual deployed artifact?
- Are readiness, release authorization, rollout completion, smoke status, and recovery recorded separately?
- Can a retirement assessment infer no consumers from missing telemetry, or infer deletion authority from shutdown approval?

## Source baseline

The packet follows the accepted local ADR records and the live tracker ADR-086 (#3791). ADR-127–130 were confirmed proposed on 2026-10-31; they are not prerequisites or authority. The local source-file hashes and checkout commit are recorded in [source-baseline.json](source-baseline.json) for review/rebase, not as ratification evidence. Before filing amendments, compare their source tracker bodies and any intervening ratification against this baseline.

The principal source decisions are ADR-038/049/050/051/052/053/073/079/080/086/110/113/121/123/125/126. The current deployment implementation boundary is documented in `backend/internal/deployreconciler/README.md` and `docs/spec/workflow-v2.md`; the live `release` workflow still delegates to a stub and declares no post-deploy acceptance stage.
