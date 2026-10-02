---
id: ADR-079
title: "Product and program management agent for human-ratified charters, release strategy, and readiness"
status: accepted
date: 2026-09-06
issue: https://github.com/kuhlman-labs/fishhawk/issues/3238
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-079: Product and program management agent for human-ratified charters, release strategy, and readiness

## Context

Fishhawk's grooming agent answers what to work on next against an established charter. The operator still supplies product direction, defines what a release should accomplish, reconciles delivery constraints, and decides when enough has shipped. Introduce an agent with a product-and-program-management remit upstream of grooming: help develop direction, propose release strategy and milestone outcomes, and assess release readiness.

Concrete example: propose that the next release lets an external team install Fishhawk and complete its first governed change without founder assistance; explain why that outcome matters, what can wait, and what evidence will demonstrate readiness. Grooming then identifies and orders the work needed to meet that promise.

Existing contracts:

- [MVP_SPEC §1](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/MVP_SPEC.md): humans set direction and approve outcomes; Fishhawk orchestrates agents, the customer's tracker remains authoritative, and Fishhawk does not become a project-management or CI/CD platform.
- [Charter §§1–6](https://github.com/kuhlman-labs/fishhawk/blob/main/.fishhawk/charter.md) and [work-management charter contract](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/work-management-v0.md#charter): the charter is currently human-authored; agents read it and never write it. It contains a north star, phase themes, non-goals, and stable rubric identifiers.
- ADR-065 #2161: grooming consumes a checked-in, version-attributed charter through the shared document-injection mechanism. The live admission/injection path requires a charter; this decision does not introduce unanchored grooming.
- **Current snapshot behavior, verified in code:** [charter_injection.go](https://github.com/kuhlman-labs/fishhawk/blob/main/backend/internal/server/charter_injection.go) resolves the default-branch head at prompt-serve time and records that commit and content hash. An amendment between run creation and prompt serve can therefore affect that run. Admission-time snapshots retained through retries are a NEW requirement of this ADR, not an existing guarantee.
- [Milestone scoping contract](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/grooming-report-v1.md): grooming already proposes included/excluded work, dependency waves, critical path, and declined scope calls. Its release definition currently admits only `source: operator_input`; it consumes the definition and must not invent it.
- [Release loop](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/deploy/release-loop.md): evidence-backed release notes, a recorded cut decision, operator-owned tag push, and publication already have an established workflow.

This ADR decides an evolution of charter authorship and release-definition provenance. Ratifying the design does not itself publish a product charter, change live permissions, commit a release date, or make this feature an alpha blocker. The persona is an interaction style; the artifacts, evidence, and authority boundaries are the product contract.

## Options

1. **Keep direction entirely manual.** Continue writing charters and release definitions by hand, with ad-hoc agent conversations outside a durable workflow. Smallest product surface; leaves recurring strategic synthesis and release coordination with the operator.
2. **Expand grooming to own strategy, prioritization, and release decisions.** One agent owns the whole process. Fewer handoffs, but it can redefine the priorities or release criteria against which its own proposals are judged.
3. **Add an upstream planning workflow with human-ratified direction.** An agent proposes charter amendments and release briefs; grooming translates approved direction into executable scope. A later readiness assessment evaluates release evidence against the approved brief and recommends a decision.
4. **Add an autonomous product manager that edits the authoritative charter and cuts releases.** Lowest operator involvement, but materially changes Fishhawk's accountability model and requires a separate decision on strategic and release authority.

## Recommendation

Adopt **option 3**. Expose a product-and-program-management workflow through Fishhawk's existing agent, artifact, gate, and audit abstractions. Keep one coherent operator experience; do not require multiple new agent personas or a separate project-management system.

### 1. Three distinct outputs

| Output | Required decision content |
|---|---|
| Charter proposal | Proposed initial charter or amendment; target users and problems; north star; desired outcomes; current-phase themes; non-goals; citable prioritization rubric; rationale and evidence for changes |
| Release brief | Release promise and audience; milestone outcomes and exit criteria; must-have versus deferrable scope; date-versus-scope policy; sequencing assumptions and capacity constraints; blocking-defect policy; required release evidence; a provisional sequence of later releases and the learning/enabling rationale for that sequence |
| Readiness assessment | Criterion-by-criterion evidence against a ratified brief and a specific candidate revision; unmet criteria and unknowns; accepted/deferred risks; recommendation to ship, narrow scope, or wait, with reasons |

The charter changes when direction changes. The release brief changes when the release commitment changes. Readiness observations do not implicitly amend either. A planning session need not amend an already-suitable charter.

**Planning horizon:** the next release becomes committed only after feasibility review and ratification; later releases remain provisional. Explain what each release delivers, what it is intended to teach us, and what it enables next. A provisional roadmap is neither a delivery promise nor permission to populate or execute future campaigns. These are logical artifact contracts; exact schema names, storage, and API shapes are subsequent implementation design.

### 2. Evidence and uncertainty

Use operator intent, available customer/discovery evidence, adoption signals, operator friction, repository state, backlog dependencies, and delivery history. Record source references and available revision/time information; distinguish observed facts, inference, assumptions, and unresolved questions. Missing evidence remains explicit.

A backlog alone is insufficient evidence of customer demand. The agent may recommend discovery work or identify a charter gap. Forecasts state their basis and uncertainty; issue counts are not a substitute for capacity evidence. The first slice accepts operator-supplied evidence and existing repository/tracker data; new external integrations are not a prerequisite.

### 3. Agent-proposed, human-ratified direction

Replace the blanket prohibition on agent authorship with an explicit proposal-and-ratification path. An agent may draft a charter or amendment as a reviewable proposal; it cannot make that proposal authoritative by itself. Charter and release-brief ratification require an explicit human decision bound to the exact proposed content and its predecessor revision.

The checked-in charter remains the authoritative grooming input. Provide one **approve-and-publish** operator action for the exact proposed content: record the human decision, then publish through the configured governed mechanism. Fishhawk adds no second strategic approval of identical content; repository protections and required reviews still apply.

Publication failure leaves an explicit **approved, publication pending** state. Retrying identical approved content against the same valid predecessor does not require another strategic decision. Changed content or a stale predecessor requires reconciliation and renewed ratification. Approval is not activation: only confirmed publication makes the charter authoritative. A proposal or approved-but-unpublished amendment must not become active direction. If publication succeeds but its acknowledgement fails, reconciliation must recognize the existing publication rather than duplicate it or overwrite newer content.

**New durable snapshot requirement:** bind existing charter and release-brief input revisions at planning or grooming run admission, before agent execution, and retain those exact inputs through retries. Missing or incompatible required inputs refuse admission; retries never silently resolve a newer default-branch head. New proposals produced during a session are versioned outputs and do not replace that session's input basis. A fresh run or an explicit change decision establishes a new basis.

Preserve commit/content-hash attribution and stable rubric IDs; retire, never recycle, IDs whose meanings change. Initial charter drafting is an explicit bootstrap mode using an operator brief and a recorded absent predecessor, since there is no charter to snapshot yet. Publish and ratify that initial charter before invoking grooming; this exception permits charter drafting only, never unanchored grooming.

Ratification covers the direction, not a blanket grant to mutate the backlog, start campaigns, waive release criteria, or execute a release.

### 4. Manager-to-groomer handoff

The manager defines milestone outcomes and proposes release boundaries. Grooming owns issue inclusion/exclusion, decomposition proposals, ordering, dependency waves, and critical-path derivation. Reuse the existing milestone-scoping machinery instead of creating a second issue-selection engine.

**Feasibility precedes release commitment:** manager proposes → grooming assesses scope/dependencies → manager reconciles tradeoffs → human ratifies and publishes → grooming produces the actionable order. Feasibility evidence identifies likely work, missing work, blocking dependencies, capacity assumptions, and unresolved questions for the ratification view. A changed proposal must carry a corresponding feasibility assessment before ratification; infeasibility is not resolved by marking the draft approved.

A feasibility pass is explicitly provisional and advisory: it uses the authoritative charter and a named draft release-brief revision, applies no tracker mutations, and cannot seed a campaign. Hypothetical charter changes remain labeled as proposals; conflicts with the authoritative charter are findings. Resolve those conflicts through charter ratification/publication and refresh feasibility against the resulting charter before committing the release. When no charter exists, the bootstrap charter must be ratified and published first. The provisional path must be distinguishable from an approved grooming handoff in both the artifact and its consumers.

The committed handoff must attribute both the charter revision and the ratified release-brief revision. **A release brief is bound to the charter revision it was ratified against.** A newer charter must not be silently paired with an older brief. Publishing a charter amendment triggers an explicit assessment of affected release commitments. Work already underway retains its approved basis until an explicit change decision; any new handoff against the amended charter requires a compatible, ratified brief. Reaffirming an unchanged release promise against a new charter is still an explicit, attributed compatibility decision. Extend release-definition provenance compatibly: preserve support for genuine `operator_input`, and represent an agent-authored, human-ratified brief honestly, including authorship and approval references. Do not relabel agent text as operator-authored merely because it was approved. Any schema changes must follow the repository's additive-versus-major-version rules.

Grooming returns infeasible scope, missing work, conflicting priorities, and unresolved tradeoffs to the manager. A resulting proposal requires a new decision before it changes the governing direction; it does not silently re-rank active work or replace an approved campaign.

### 5. Release readiness and execution

Establish release criteria before evaluating readiness. Assess a specific candidate commit/component revision set and the approved brief, with links to available acceptance, review, installation, recovery, and compatibility evidence as applicable to that release. Missing or stale evidence is unknown, never a passing criterion.

The agent can recommend a smaller release with explicit deferrals. If that recommendation changes the approved promise or criteria, ratify the revised brief before treating the candidate as ready under it. A changed candidate or brief requires a refreshed assessment. This prevents moving the finish line merely to justify shipping.

A readiness recommendation feeds the existing release loop. It does not cut a version, push a tag, publish, or execute deployment as a side effect. Existing release authorities and execution mechanics remain governing; autonomous release authority would need a separate decision.

### 6. Feedback and change discipline

Start planning on demand. Revisit direction on an explicit operator request or a recorded material change: discovery evidence, delivery constraint, failed milestone assumption, or post-release outcome. Each proposal identifies the trigger, the change from the prior commitment, and its downstream impact. Unchanged evidence should yield no material amendment.

After release, compare observed outcomes with the release promise and propose the next adjustment. Missing outcome data is a finding. Initial support can use operator-supplied observations; continuous monitoring is outside this decision.

### 7. Delivery sequence after ratification

First slice: an on-demand planning conversation produces a charter proposal when needed and one release brief with a provisional longer-term horizon; runs advisory feasibility before release commitment; supports human approve-and-publish with recoverable publication; and supplies a compatible, ratified handoff to grooming. Prove that unratified output cannot become authoritative input or seed execution, that feasibility performs no tracker mutations, and that admission-time input snapshots survive retries and intervening charter changes.

Subsequent scope: candidate-bound readiness assessments connected to the existing release loop, then outcome feedback and material-change triggers. Do not add new workflow stage types, automatic campaign execution, customer messaging, or an independent tracker.

Create the epic and implementation issues from this ratified decision. Scheduling is a separate prioritization decision; acceptance does not make the feature an alpha release blocker.

### 8. Validate management judgment as well as workflow correctness

The eventual epic must include evaluation cases for unnecessarily broad releases, defensible deferral, missing customer evidence, infeasible dependencies, and unchanged evidence. Judge whether recommendations are evidence-grounded, surface uncertainty, explain tradeoffs, and avoid material priority churn when nothing changed. Use explicit behavioral criteria and counterexamples; exact prose or one preferred ranking is not the oracle. Pair structural/offline checks with a real-agent evaluation and report them separately: schema validity and successful approval do not demonstrate sound management judgment.

## Decision

**Ratified 2026-09-06 by Brett (repository maintainer), on the explicit instruction “amend the ADR then ratify” following review of this proposal.** The agent records the maintainer's decision; it is not self-approval.

Adopt **option 3**, with the amended Recommendation §§1–8 as the binding design:

1. An upstream product-and-program-management workflow produces charter proposals, release briefs, and readiness assessments; grooming owns work-item scoping and ordering; the existing release loop owns release execution.
2. The next release becomes committed after feasibility and ratification; later releases remain a provisional strategy horizon with explicit learning and enabling rationale.
3. A provisional, non-mutating grooming feasibility pass precedes release-brief ratification. It uses the authoritative charter, identifies its draft inputs, and cannot feed campaigns.
4. Charter authorship becomes agent-proposed and human-ratified. One approve-and-publish action binds exact content and predecessor, honors repository protections, and recovers publication failures without a redundant strategic decision for unchanged valid content.
5. A ratified release brief binds its charter revision. Charter changes require an explicit compatibility/change decision for affected commitments; active work is not silently rebound.
6. Planning and grooming bind existing inputs at run admission and retain them through retries. This strengthens today's prompt-serve-time charter resolution and must be implemented and tested as new behavior. Initial charter drafting records an absent predecessor and remains distinct from grooming.
7. Release-definition provenance distinguishes agent authorship from human ratification. Readiness is bound to a candidate and approved criteria; it recommends a decision and grants no release execution authority.
8. Validate management judgment with behavioral scenarios as well as validating artifact, authority, publication, snapshot, and handoff correctness.

**Sequencing:** first deliver on-demand planning through feasibility, ratification/publication, and the grooming handoff; then readiness and outcome feedback. Create the implementation epic and child issues from this decision. Release scheduling remains separate.

**Scope of this ratification:** the architecture and workflow policy above. It does not approve a concrete product charter, release brief, backlog mutation, campaign, tag push, or deployment. Existing live controls change only through the subsequent implementation work.

**Implementation tracking:** [E71 — Product and program management](https://github.com/kuhlman-labs/fishhawk/issues/3240), with 12 dependency-linked children. All implementation work is in Backlog; no campaign or implementation run was started by this ratification.

## Consequences

- Product direction, release promises, backlog scope, and release recommendations become attributable decisions with explicit handoffs. The operator reviews concrete tradeoffs instead of assembling every input.
- The manager can challenge the existing backlog and recommend deferral or discovery. Its judgment remains limited by available evidence; human approval does not make an unsupported forecast reliable.
- Additional artifact, approval, activation, provenance, and durable admission-snapshot seams are required. The implementation must demonstrate the end-to-end handoff, including initial-charter creation, provisional feasibility, rejected/stale proposals, recoverable publication, incompatible charter/brief pairs, and preservation of active-run inputs through retries.
- Approval fatigue, strategy churn, backlog bias, and self-justifying release criteria are primary product risks. Mitigations are evidence attribution, minimal amendments, explicit exclusions, pre-agreed release criteria, and separation of proposal from activation.
- The charter's human-authorship wording, the work-management reference/default commentary and mirrors, relevant prompt/document-injection contracts, and milestone release-definition provenance need coordinated changes under the eventual epic. The architecture is ratified; those live contracts remain intact until the corresponding implementation lands.
- The charter remains checked-in Markdown with citable rubric IDs; the tracker remains the source of work-item truth; release execution continues through the existing governed release/deploy surfaces.
- Reversal: disable the planning workflow and resume manual charter/release-brief authoring. Already-ratified charter content remains valid, and historical artifacts and decisions retain their attribution.

**Done-means — satisfied:** the maintainer's ratification is recorded in Decision, resolving authority, feasibility, publication, revision binding, and delivery boundaries. The final record is the basis for the implementation epic; implementation completion is not required to close this decision record.

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
