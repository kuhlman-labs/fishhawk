---
id: ADR-135
title: "A Captain-approved public roadmap, published first to the site"
status: accepted
date: 2026-11-03
issue: https://github.com/kuhlman-labs/fishhawk/issues/4920
supersedes: []
superseded_by: []
amends: ["ADR-065"]
applies_to: ["backend/internal/roadmap/**", "backend/internal/workmgmt/**", "backend/internal/server/*roadmap*", "docs/spec/*roadmap*"]
---

# ADR-135: A Captain-approved public roadmap, published first to the site

## Context

External readers need 10–20 understandable outcomes, stable wording and horizons rather than dates.

The repository declares user-owned Project #7 (`.fishhawk/work-management.yaml:11`). Its charter identifies themes T7–T12 (`.fishhawk/charter.md:84–102`); N2 makes the tracker authoritative and the board a projection (`.fishhawk/charter.md:130`). Grooming already supports charter-attributed proposals and audited apply under [ADR-065](docs/adr/065-backlog-grooming-prioritization-agent.md). The non-delegable `milestone` precedent was added by E54.9 / #2309 (`docs/spec/workflow-v2.md:1354`).

[ADR-079’s planning horizon](docs/adr/079-product-and-program-management-agent.md:52) distinguishes a ratified next-release commitment from provisional later releases; a public roadmap is not a delivery promise.

Existing machinery does not supply this roadmap. Actor-attributed activity history and durable cross-scan dispositions need new work. Independent public cards require a new provider capability. `fishhawk_release_notes` previews or persists notes without resolving epic-to-release associations (`backend/internal/mcpserver/release_notes.go`).

**UNVERIFIED offline:** the approximate 2,700-item inventory, current board contents, Project #8 availability, and GitHub’s project-level rather than view-level visibility boundary. Operator verification precedes any project activation.

## Options

1. **Public view on #7:** low setup cost; cannot establish a confidentiality boundary without independently verified view isolation.
2. **Site only:** approved, generated content through the repository’s publishing path; omits the requested GitHub experience.
3. **Site first, then separately designed project and release transitions:** delivers a reading surface before introducing project credentials, reconciliation and release associations.
4. **Managed project first, site later:** gives GitHub-native readers access sooner, but requires the new draft-card, reconciliation and credential capability first.
5. **Manual public project:** quick to curate, but duplicates maintenance and lacks tested governance.

## Recommendation

Option 3. Decide Phase 1 in full. Ratify Phase 2 and Phase 3 only as directions, with amendments required before their implementation slices begin.

## Decision

**Ratified by the Captain 2026-11-03: Option 3, site-first.** Phase 1 (D1–D5 and D8 below; D6 and D7 became the Phase 2 and Phase 3 directions) is decided in full; Phase 2 (public project) and Phase 3 (release association and Shipped) are ratified as direction only and require amendments P2 and P3 before their implementation slices begin.

The decision covers the declaration, approved site snapshots, non-delegable grooming, site confidentiality and withdrawal, publication enforcement, and Phase-1 upkeep. The project and release sections below settle direction and constraints, not implementable contracts.

**D1. Declaration.** Add an optional, closed `roadmap` object to `work-management-v0`; absence disables new publication.

| Field | Contract |
|---|---|
| `include` | Required `{type, label}`; both match. `type` validates against the declared `types:` keys. This repository declares type `epic` and label `roadmap`. |
| `exclude_labels` | Optional additional exclusions; no product-default label vocabulary. |
| `voice_terms` | Optional path to the repository’s declared voice term list; absent means no product-side voice check. |
| `max_active` | Positive integer, default 20; counts approved `now`, `next`, `later` entries. |
| `stale_after_weeks` | Positive integer, default 4. |

Project configuration and release transitions are outside this declaration until their amendments.

Enforce `max_active` atomically across concurrent approvals. Label removal, type mismatch or declaration removal revokes publication eligibility and starts site withdrawal; re-enabling requires fresh approval. Retain site publication identities for cleanup.

**D2. Record and approval.** Persist repository-scoped projection UUID, private epic reference, monotonic revision, public title/summary, theme ID and approved label text, horizon, evidence references and site publication identities. Public text is plain text; dates and deadlines are absent. Phase 1 permits only `now`, `next` and `later`.

Approval states are `pending → approved → revoked`; revoked revisions never revive. Amendments create pending successors. D2 approval is the `roadmap`-class grooming gate’s approval, with no separate approval verb. Eligibility is the gate’s repository-declared `approvals.members`, intersected with [ADR-123 Decision 1](docs/adr/123-forge-verified-stage-approvals.md) and [ADR-126 D5](docs/adr/126-security-finding-record.md), never a product identity constant.

Site publication is per immutable repository snapshot: snapshot ID, monotonic revision, ordered vector of `(projection ID, revision)`, revocation generation and predecessor snapshot ID. At ingest, the backend derives the pending snapshot from grooming-report entries, recomputes its canonical digest and refuses a differing stated digest. The gate binds the server-computed digest through the report artifact.

Bind approval to expected revision, site target, pinned charter, relevant evidence and canonical snapshot digest. Canonical encoding is versioned UTF-8 JSON: sorted keys and entries, fixed escaping, integers only, no insignificant whitespace; hash with SHA-256. The backend manifest binds only these exact JSON bytes, the approved artifact.

Payload, target, authorization, classification or relevant evidence changes invalidate approval. Compare-and-swap approval/apply, capacity and outstanding-snapshot checks are transactional. Publication serializes against the site’s last merged snapshot, never its last approved snapshot; an older snapshot cannot overwrite a newer one.

While an approved-but-unmerged site snapshot **or an unmerged D4 revocation snapshot** exists, grooming-gate approval refuses with `roadmap_snapshot_outstanding`. The refusal persists until that snapshot merges, or until its revocation or supersession reconciles with no unreconciled dispatch claim remaining; a replacement revocation snapshot remains outstanding. This limit never prevents D4 safety withdrawal. Operator revocation of an approved-but-unmerged snapshot closes or withdraws its PR, pushes nothing, changes no entry’s eligibility and is audited; it clears `roadmap_snapshot_outstanding` once no unreconciled dispatch claim remains; a reconciled MERGED outcome records the snapshot as merged and as the predecessor.

Retired themes raise a finding requiring Captain re-theming; exports retain the last approved label, never substitute current charter prose.

**D3. Grooming.** Register `roadmap` as `report|gated`, never `auto`, with both independent delegation refusals. Additions, wording, themes, removals and discretionary `now|next|later` moves require D2 approval and charter theme/rubric citations. A discretionary `now` proposal also cites the ratified release brief where one exists. Shipped is entirely outside grooming and unavailable in Phase 1.

Issue text reaches the propose prompt only through the quarantined issue-context path (`backend/internal/prompt/prompt.go`). Hygiene cannot mutate roadmap fields. Run the `roadmap.voice_terms` list at proposal and approval when declared; absence disables these product-side checks. This repository also runs `scripts/check-site-voice`. Repository vocabulary remains declared configuration under [ADR-110](docs/adr/110-repository-agnostic-product-features.md).

**D4. Site confidentiality and withdrawal.** Apply ADR-126’s native security classification and the forge confidential marker before proposal, approval and publication, including referenced children and historical payloads. Repository exclusions only strengthen this floor. Unknown classification or incomplete enumeration blocks new publication.

Classification events and reconciliation trigger durable revocation tombstones, invalidate queued exports/approvals and fence workers by revocation generation. Immediately before export, push and publication deployment, refresh classification through a separate guard and recheck authorization. The exporter itself never reads the tracker. Removal never waits on classification and must succeed during a classification outage.

Revoking an approved-but-unmerged snapshot supersedes it, cancels its merge-queue intent and closes its PR. Extend the existing withdrawal machinery: today’s handler supports yielded entries and refuses live or claimed entries, so it does not already provide general revocation cancellation (`backend/internal/server/merge_queue_withdraw.go:116`). In-flight outcomes require reconciliation.

Build and push a **revocation snapshot** from the last merged site snapshot minus tombstoned entries ONLY when it contains a tombstoned entry; otherwise push nothing, including before first publication. Completion requires BOTH (a) no unreconciled `dispatched` or `dispatch_failed` claim, on any path, on any superseded snapshot’s publication PR, AND (b) confirmed deployed content excluding the tombstoned entry or confirmed unpublication ((b) holds vacuously only while no site snapshot has ever merged; an unconfirmed deployment is not 'no deployed content'). Until both hold, keep withdrawal open with retries and failure paging. A reconciled MERGED outcome reopens withdrawal against the newly merged predecessor. Revocation authorizes the snapshot without fresh approval. Its pre-push check requires a strict subset of the last merged predecessor’s entry set, byte-identical surviving entries, and fixed withdrawal-template Git text under D5. Anything else refuses; revocation never authorizes additions or restoration.

Withdrawal begins when exclusion is detected: suppress exports, cancel pending publication and prepare a deletion-only site change. Retry durably with bounded backoff; page the operator on the first failed withdrawal or unavailable classification check. If site removal cannot deploy promptly, unpublish the affected page. Recheck remote outcomes after in-flight writes; retain tombstones through retries. Already-public branch history and third-party copies cannot be recalled. Logs carry identifiers, not excluded payloads.

These completion rules concern the site only. They establish no completion shortcut for future project writes.

**D5. Site publication.** Add authenticated `GET /v0/roadmap/export?repo=owner/name&snapshot=` with new scope `read:roadmap-export`. Scopes are flat per account; the record binds the repository, not the token. Require account ownership and a matching `repo`/snapshot. Cookie-session callers bypass bearer scope checks but retain account and applicable repository-read visibility checks, following `backend/internal/server/middleware.go` and `docs/api/v0.md`.

The endpoint returns only persisted, approved public fields and opaque identities/revisions. Revoked snapshots refuse, except for D4-authorized revocation snapshots.

`site/roadmap.json` is a committed generation input, excluded from served assets. Deterministically render `site/src/content/docs/roadmap.md` between `BEGIN/END GENERATED roadmap` markers. Astro uses committed content without live board queries.

**Roadmap publication content** is `site/roadmap.json` plus bytes inside those markers. An empty generated region is not publication content. Any commit touching it is a publication commit, whatever the workflow. A default-on product constraint enforces this regardless of stage type, following `agent_instruction_paths` (`docs/spec/workflow-v2.md:1044`). The runner enforces it pre-push, with the backend check as a backstop; unresolved stage type fails closed only for a pushed range touching publication content. A publication PR’s whole commit range may touch only publication content; other files or prose changes refuse. Outside prose remains editable through ordinary PRs under normal review.

Before **any public push touching publication content**, require the JSON digest to equal the approved digest and byte-exact docgen drift against the pinned renderer from the merge base. Renderer changes in the range refuse. Markdown is pinned by that test, not by a backend Markdown manifest. The ordinary repo-wide drift test excludes the roadmap region; the pre-push check and re-render publications own renderer currency. All public Git text—commit messages, PR title and PR body—is template-generated, containing only fixed text plus snapshot ID, revision and digest. Refuse agent-authored commit-message sidecars (`runner/cmd/fishhawk-runner/main.go`, `implementCommitMessage`) and free PR text for publication commits. Validate the **entire newly published commit range**. D4 withdrawals use the fixed withdrawal template and revocation authorization.

Add a pure-git verify leg on the [ADR-090 path](docs/adr/090-verify-the-merge-candidate.md) at the rebased head: the pushed JSON’s predecessor snapshot ID must equal the snapshot ID committed at the merge base. `site/roadmap.json` absent at the merge base ⇔ a null predecessor, admissible only for the site’s first snapshot. A re-render publication requires byte-unchanged JSON, the merge-base renderer’s region output and template Git text; byte-identity with merge-base JSON passes the predecessor leg.

Add a shared product-state approval/revocation leg to **both** `mergeCandidateGate` and `evaluateMergeQueueEligibility` (`backend/internal/server/merge_run.go:1233`; `backend/internal/server/merge_queue_eligibility.go:266`). Evaluate it before any forge-probe early return, including `mergeCandidateGate`’s `warnUngated` paths. For publication content, this leg never fails open: indeterminate authorization refuses. An ordinary snapshot must still be approved and not revoked; D4 governs revocation snapshots.

For any run whose pushed range touches publication content, every dispatch path—inline `merge_run`, queue merge-on-pass and delegated `may_merge`—uses **only the pinned synchronous `MergePullRequestAtHead` arm**. If the PR is not clean, defer the merge. Re-evaluate the approval/revocation leg immediately before that synchronous call. Invalidated publication PRs are withdrawn, never merged.

`EnableAutoMerge*` must refuse for such runs, including the paths through `QueueMerge` and `ForgeMerger`; pinning an auto-merge does not satisfy this decision (`backend/internal/githubclient/client.go:1645`; `backend/internal/server/forge_merger.go:404`, `:447`). No later forge auto-merge may outlive the product-state check.

The deployment recheck belongs **only in the operator-installed `docs.yml` `deploy` job**, never `build`. The current workflow separates dependency execution from deployment authority (`.github/workflows/docs.yml:28`, `:70`; `site/README.md`, Pages workflow). Compare the candidate deployment SHA’s `site/roadmap.json` with the last successfully deployed one; skip the export recheck when they are equal. For a changed publication, call the export endpoint immediately before deployment and refuse revoked or otherwise unauthorized content, except for D4-authorized withdrawal. The recheck fails closed only for deployments touching publication content; unrelated site deployments do not acquire an export-service dependency.

**Operator preconditions, UNVERIFIED until installed:** a reachable export URL and a `read:roadmap-export` secret available only to the `deploy` job. Keep the secret out of the dependency-running `build` job.

Public branches precede merge review (`docs/ARCHITECTURE.md:63`); the normal PR is a second review. Require byte-exact drift, voice, sidebar, reference-integrity, build and internal-link checks. Display snapshot revision and track deployment separately from approval and merge. Deployment is confirmed only by a successful forge `github-pages` deployment whose SHA is the merged SHA or a default-branch descendant of it, observed by the backend; a later `inactive` on a once-successful deployment keeps it confirmed; absent, unknown, pending or failed status is not confirmed. This applies [ADR-111 D1](docs/adr/111-documentation-freshness-for-the-published-site.md), adding no amendment edge.

**D8. Phase-1 upkeep.** Add source `roadmap`, pinning scan time, source revisions and enumeration boundaries. Flag:

- `now|next` entries with zero open children, including zero total children;
- `now` entries inactive for the configured interval;
- approved/committed-site/deployed-state drift, a roadmap region stale against the current renderer and retired themes.

Activity spans epic and direct structural children: child completion, a merged PR whose run’s issue is a child, or a human comment. Exclude synchronization actors; use approval time when no qualifying activity exists.

Add activity-history and projection-evidence contracts beyond today’s run/file references (`backend/internal/workmgmt/reader.go`; `backend/internal/plan/upkeepreport.go`). Truncation or unresolved actors produce named degradation.

Cross-scan dispositions are new work: key by repository, projection, check and meaningful evidence revision. Clock rollover alone does not reopen a disposition; changed facts or resolution followed by recurrence do. Approved upkeep findings file issues; separately authorized runs prepare PRs (`backend/internal/server/upkeep_apply.go`).

## Directions requiring amendments

These directions are ratified with this ADR; their implementation contracts remain open and are not reviewed here for implementability.

**Phase 2 — Separate public project.** Publish independent cards containing approved fields only, never internal issue objects or links. Verify target visibility, mappings and credentials before activation; unsupported capabilities refuse explicitly. Remote state may verify delivery but never supplies roadmap intent or run decisions.

A reconciliation-only exception to [ADR-064 Decision 2](docs/adr/064-forge-project-board-work-queue-input.md) is a candidate, preserving its derived-projection boundary and Decision 3’s MCP prohibition. No ADR-064 amendment is made here.

**Amendment P2 must settle:** the confirmed-delivery predecessor and outstanding limit; withdrawal after partial or ambiguous writes; independent site/project progress without one stalled destination blocking the other; and exact ADR-064 amendment text. In particular, confirmed S3 may contain `{A}` while unconfirmed S4 has already created public X before another write fails. X’s absence from S3 cannot establish successful project withdrawal. The amendment must resolve and test that case.

**Phase 3 — Release association and Shipped.** Shipped is an event edge from `release_published` (`backend/internal/server/release_publish.go`), never a grooming class and never `objective_reversible`, whose evaluator is absent (`docs/spec/workflow-v2.md`, “Backlog-grooming action classes”). It requires approved public wording and confirmed release evidence; closure, previewed notes and textual matching alone never suffice. Reversal is conservative and must not invent public wording. Release-changelog export remains ADR-111 Direction B’s separate design.

**Amendment P3 must settle:** the association record and its approval gate, the exact permitted retraction delta, and interaction with project delivery.

## Consequences

Phase 1 establishes a deliberate site publication boundary with durable approvals, revocation, synchronous merge enforcement and separate deployment tracking. Initial entries require approval; existing labels grant none. It makes no project writes and publishes no Shipped claims.

Publication depends on implemented ADR-126 controls, not its ratification alone. Counterfactual tests must fail when approval, confidentiality, retry or pre-push guards are removed.

Activation requires the operator-installed deploy recheck, reachable export URL and deploy-job-only credential. **UNVERIFIED until installed.** Project and release implementation remain blocked on amendments P2 and P3.

## Record metadata

- **Record metadata.** amends: ["ADR-065"]; supersedes: []; applies_to: ["backend/internal/roadmap/**", "backend/internal/workmgmt/**", "backend/internal/server/*roadmap*", "docs/spec/*roadmap*"].

ADR-065 gains the non-delegable class. ADR-110, ADR-123 and ADR-126 supply existing rules; ADR-111 D1 supplies the generated-region pattern. Applies ADR-079’s planning horizon; no edge. Any ADR-064 amendment edge belongs to amendment P2.

## Implementation slices

### Phase 1 — Approved projection, grooming and generated site

**1. Declare and persist the projection.**

- **Scope:** `docs/spec/work-management-v0.{md,schema.json}`; new roadmap schemas and `backend/internal/roadmap/`; `backend/internal/workmgmt/`; `backend/internal/postgres/migrations/`; schema mirrors and `scripts/sync-schemas`.
- **Done-means:** strict declarations, including `include.type` validation against `types:`; revisioned pending/approved/revoked records; immutable site snapshots with ordered projection/revision vectors, revocation generations and last-merged predecessors; canonical JSON digests/manifests; atomic capacity enforcement; private/public separation; persistent tombstones and revocation snapshots; disabled-by-absence behavior; a non-Fishhawk fixture through production resolution. Concurrent approvals and out-of-order publication cannot overwrite newer snapshots.
- **Dependencies:** ratification; ADR-126 classification and confidential-storage controls before activation. New paths are proposed implementation surfaces.

**2. Bind roadmap grooming-gate approval to publication authorization.**

- **Scope:** grooming-report schema/reference/mirrors; workflow-v2 schema/reference and both embedded mirrors; `backend/internal/plan/`, `backend/internal/spec/v2autonomy.go`, CLI validation counterparts, server grooming handlers, `backend/internal/prompt/`, report-class-to-action-class dispatch in `backend/internal/workmgmt/grooming_apply.go`, and audit-category registration.
- **Done-means:** independent `auto` refusals; D2 approval through the existing grooming gate with repository-declared members and credential predicate; report-artifact binding to the server-computed snapshot digest, refusing ingest mismatches; no separate approval verb or hygiene/Shipped bypass; quarantined intake; declared-source voice checks; confidentiality before prompts and publication. Negative controls cover delegated subjects, stale revisions, historical security payloads and reclassification. Test `roadmap_snapshot_outstanding` for both approved publication and unmerged revocation snapshots, including replacement revocations.
- **Dependencies:** slice 1; operator-authored workflow/voice declarations. Prompt changes stale catch-rate evidence and require operator recording. The roadmap groomer owes no ADR-110 rule 3(b) arm. Observe the record freeze until the 2026-11-01 re-pin stated in [ADR-110’s Status](docs/adr/110-repository-agnostic-product-features.md:14). ADR recording itself stales the architect arm (`scripts/check-review-prompt-eval:52–54`).

**5. Export, generate and gate the site.**

- **Scope:** new export handler and authorization; `backend/cmd/fishhawkd/token.go`, `backend/internal/oauthas/scopes.go`; `docs/api/` and generated API reference; roadmap page skeleton; `site/astro.config.mjs`, `site/README.md`; `cli/internal/docgen/`; generation scripts; runner/server pre-push checks; predecessor verify leg; `backend/internal/server/merge_run.go`, `backend/internal/server/merge_queue_eligibility.go`, `backend/internal/server/merge_queue_withdraw.go`, `backend/internal/server/forge_merger.go`, `backend/internal/githubclient/client.go`; deployment authorization; backend observation of a successful forge `github-pages` deployment at the merged SHA or a default-branch descendant (D5).
- **Done-means:** ship an empty generated-region skeleton and required sidebar entry, with no `site/roadmap.json`. Implement repository/snapshot binding, flat account scope and cookie-session behavior; tracker-independent export; deterministic escaping and markers; digest/drift verification; fixed commit/PR templates; entire-range enforcement. Refuse unapproved commit messages, outside-marker injection, renderer substitution and `feature_change` publication bypasses. Test a renderer PR landing followed by successful re-render publication; refuse re-renders also changing JSON.
- **Merge controls:** add the fail-closed product-state leg to both gates before forge-probe early returns, and recheck immediately before synchronous dispatch on all three paths. A publication PR not clean at admission receives no auto-merge and is deferred. Exercise `EnableAutoMerge*` refusal, including `QueueMerge` and `ForgeMerger`, and the `warnUngated` paths.
- **Withdrawal and deployment controls:** test revocation after green verify with unchanged main; queued publication revoked before pumping; concurrent approvals; stale S4 after S5 and revocation S6 = S5-X; in-flight writes followed by withdrawal; merged S2 without X failing deployment while S1 with X remains deployed; X revoked during in-flight `MergePullRequestAtHead`; no-push cancellation for unmerged-only entries and before first publication; removal during classification outages. Revocation snapshots remain outstanding until resolved. Test the deploy-only recheck, unchanged-JSON skip, publication-only failure boundary and absence of the export secret from `build`. JSON is absent from built assets.
- **Dependencies:** slices 1–2; operator installs CI/deploy controls and completes the [Auth change checklist](AGENTS.md)’s token-migration dry run. ADR-111’s `scripts/check-site-references` and `.github/workflows/ci.yml` `site` job are not landed; availability remains **UNVERIFIED at activation**. Update generated-page descriptions and inventories in `AGENTS.md`, generation scripts and `cli/internal/docgen`, preserving the four reference pages. `.agents` skills were checked and need no change.
- **Gate integration:** register new verify legs/harness inputs in `backend/internal/basedelta/verifylegs_test.go` and the live `verdict_transfer` declaration; update its fixture as required. Generated references and MCP guard tests remain in scope.

**6a. Add site upkeep and activate Phase 1.**

- **Scope:** upkeep schema/reference/mirrors, `backend/internal/upkeep/`, server evidence/disposition adapters, activity-history capability, prompt integration and operational documentation.
- **Done-means:** zero-open-child, inactivity, theme and site-drift findings; complete pagination and explicit degradation; durable cross-scan dispositions; approved refresh issues without implicit run creation.
- **Dependencies:** slices 1–2–5; operator-authored `.fishhawk/**` declarations, CI site path coverage and deploy controls. Captain approves initial snapshots through D2. Required prompt-evaluation recording is an operator dependency, never implement-agent done-means.

### Phase 2 — Separate public project

**3. Implement independent-card synchronization.**

- **Status:** blocked on this ADR's amendment P2.
- **Dependencies:** Phase 1; amendment P2; operator verification of GitHub capabilities, credentials and proposed Project #8, “Fishhawk Roadmap.” Live identity and availability remain **UNVERIFIED**.

**6b. Extend upkeep and activate project synchronization.**

- **Status:** blocked on this ADR's amendment P2.
- **Dependencies:** slice 3; amendment-defined delivery and approval contracts; operator credentials and field setup; Captain approval of project publication.

### Phase 3 — Release association and Shipped

**4. Implement the gated association and event-driven transition.**

- **Status:** blocked on this ADR's amendment P3.
- **Dependencies:** preceding phases; amendment P3; operator-enabled release transitions; verified immutable-release compatibility required by ADR-111 Direction B. Do not assume its separate changelog design is implemented.

**6c. Complete release upkeep and activation.**

- **Status:** blocked on this ADR's amendment P3.
- **Dependencies:** slice 4; amendment-defined association approval, retraction and delivery contracts; Captain approval and operator activation.
