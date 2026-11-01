# Proposed ADR-079 amendment: experiments and lifecycle evidence in product planning

**Draft v1 — 2026-10-31. Not ratified.** Target: [ADR-079](../../adr/079-product-and-program-management-agent.md), tracker [#3238](https://github.com/kuhlman-labs/fishhawk/issues/3238). Proposed form: an in-body amendment clarifying Recommendation §§1–2, 4–6, and 8. The accepted body remains authoritative until the Captain ratifies this text. No new ADR number is needed for this amendment.

## Context

ADR-079 already owns evidence-grounded direction, release briefs, feasibility, candidate-bound readiness, and post-release outcome feedback. Its first slice accepts operator-supplied observations and existing repository/tracker data. The missing specificity is how uncertainty is resolved before a release commitment and how a release brief describes readiness across development, qualification, and operation.

This amendment defines evidence requirements within those existing outputs. It does not introduce an experiment service, new stage type, participant-management system, or independent approval ledger. ADR-052 continues to own draft work items; ADR-084 and ADR-113 continue to own technical review and architectural decisions.

## Options

- **A. Leave evidence expectations implicit.** Smallest change, but a prototype demo or model opinion can be mistaken for user validation, and operational obligations can surface only after implementation.
- **B. Clarify the existing planning and readiness contracts (recommended).** Experiments are ordinary governed work and their observations are planning inputs with explicit limits.
- **C. Add a dedicated research workflow engine and new artifact/state family now.** Deferred until recurring use demonstrates a missing capability. Not required to complete the initial lifecycle.

## Recommendation

Adopt option B by appending the following amendment to ADR-079 after ratification.

### Amendment A1 — experiments, operational criteria, and evidence handoffs

**A1.1. Uncertainty is a planning input.** A charter proposal or release brief distinguishes observed problems, inferred causes, untested assumptions, and committed requirements. For each material uncertainty, the manager recommends accepting the risk, narrowing the promise, or commissioning a bounded investigation. It cannot turn an assumption into a fact because the backlog already contains work based on it.

**A1.2. Investigations have a decision question.** When an investigation is needed, its ordinary issue or approved plan names the hypothesis/question, alternatives, participant or target population where relevant, evidence to collect, success/failure/inconclusive criteria, time/cost bound, stopping condition, owner, and the planning decision it informs. Criteria are fixed before observations are evaluated. A revised criterion is a new attributed revision, not a rewrite of the earlier experiment's result.

Evidence names its source, artifact/prototype revision, observation window, relevant environment, and known coverage limits. Small samples, synthetic scenarios, and model-generated personas are identified as such. Missing or contradictory evidence remains unresolved. A successful technical demonstration is not evidence of customer demand.

**A1.3. Existing workflows carry the work.** ADR-052 drafts and gates experiment work items. Code prototypes use ordinary governed code-change workflows, in disposable branches or non-production targets as their approved scope permits; this amendment creates no exception to merge, instruction-path, or credential controls. Runtime checks may use ADR-049 acceptance where its supported inputs and containment fit. An investigation that needs no code can consist of operator-supplied observations and a planning session.

The first delivery ingests evidence references through ADR-079's existing proposal contracts. A new typed experiment artifact, automatic recruitment, background research, or a new execution primitive needs a subsequent design decision. No pseudo-YAML for such features is implied here.

**A1.4. User evidence and technical evidence answer different questions.** Independent acceptance can establish that a declared behavior works. Usability/comprehension claims require attributed observations from intended users or an explicit statement that no such observations exist. An agent may summarize observations and propose follow-up questions; it cannot substitute its own simulated experience for a user study. Recruitment, consent, communications, and access to recordings remain operator-owned. Raw personal data stays in access-controlled research storage; Fishhawk carries bounded references and appropriately redacted summaries.

**A1.5. Material design choices follow the ADR process.** Architectural alternatives, API/data compatibility, migration strategy, trust boundaries, and recovery assumptions are identified before committing implementation scope. Settled decisions are cited; unresolved architectural choices go through ADR-113. An architect persona's advisory verdict is not ratification. Product feasibility does not silently settle an open architecture question.

**A1.6. A release promise includes operating evidence.** In addition to user-visible outcomes, the brief identifies the applicable installation/upgrade, interoperability, accessibility, performance/capacity, security, migration, backup/restore, recovery, documentation, and support obligations. It names which are required, their measurable criteria and evidence owners, and the reason an otherwise relevant category is inapplicable. No universal checklist is added to every small change; the release's promises and risks determine coverage.

Required evidence is attributed to the layer that can establish it: per-change preview, integrated dev, candidate qualification in stage, bounded production smoke, or post-release observation. A production observation cannot be claimed before production use. A service objective includes its measurement source and owner; Fishhawk does not become the monitoring system.

**A1.7. Handoffs remain revision-bound.** The evidence chain is charter/brief revision → approved work items/plans → merged changes → candidate/qualification references → readiness decision → deployed release → outcome observations. Existing records remain authoritative for their own facts. A release view may join their references but must preserve source permissions, missing links, stale observations, and changes to criteria. It is not a second tracker or a new authority above campaigns.

Where the companion [promotion draft](https://github.com/kuhlman-labs/fishhawk/blob/draft-adr-release-promotion/docs/proposals/sdlc-lifecycle/03-release-candidates-and-promotion.md) is later ratified and implemented, readiness consumes its exact candidate and qualification identities. Until then, ADR-079's existing specific-candidate requirement remains; this amendment does not declare the new manifest implemented.

**A1.8. Outcomes have an evaluation plan before release.** A brief names expected outcomes, available baseline, observation window, source, accountable reviewer, and a trigger to revisit direction. No baseline means an exploratory observation, not a quantified improvement claim. After release, the assessment distinguishes measured results, insufficient data, confounding changes, and inference. Outcome findings feed existing refinement/grooming and ADR-125 lessons as appropriate. They do not automatically amend the charter, start campaigns, or change delegation.

**A1.9. Scope and authority stay unchanged.** Proposals remain agent-authored and human-ratified under ADR-079. Feasibility precedes commitment; publication is distinct from approval; retries retain their admitted input revisions. This amendment grants no production experimentation, customer messaging, automatic tracker mutation, or release execution authority. It adds no alpha exit criterion beyond ADR-080's existing role obligations.

## Decision

Pending Captain review and ratification under ADR-113. The proposed A1 text above has not been adopted and must not be appended to the accepted record as a settled amendment yet.

## Consequences

The planning feature can close the idea-to-evidence and production-to-learning loops without another workflow engine. The cost is clearer evidence provenance, readiness criteria, and operator review; evidence quality still depends on the observations supplied.

Implementation belongs to E71 (#3240), with scope reviewed against its existing children rather than duplicating the epic. Any new schema/storage/API contract is planned explicitly. Review should keep A1 within ADR-079's existing output contract; if a new experiment execution service is desired, it is a separate ADR.

### Validation

- A prototype works technically but observed users cannot complete the task: the system cannot label the user outcome proven.
- No customer observations or baseline exist: the report surfaces uncertainty without inventing demand or savings.
- Criteria change after a failed experiment: historical results remain bound to the old revision.
- A release needs a migration and a recovery procedure: missing evidence remains unknown in readiness.
- Observed outcomes contradict the promise: a revision is proposed through the existing human-ratification path.
- A non-Fishhawk fixture supplies different evidence categories and sources through declarations; no repository-specific paths or environment names are inferred.
