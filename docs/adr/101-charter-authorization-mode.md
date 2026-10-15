---
id: ADR-101
title: "Charter authorization mode: lease-based, bounded autonomous operation by a product-spawned operator under a deterministic engine, intersected with delegation; captain seat stays human (amends ADR-040/085/089/097)"
status: accepted
date: 2026-10-14
issue: https://github.com/kuhlman-labs/fishhawk/issues/4412
supersedes: ["ADR-040", "ADR-089", "ADR-097"]
superseded_by: []
applies_to: []
---

# ADR-101: Charter authorization mode: lease-based, bounded autonomous operation by a product-spawned operator under a deterministic engine, intersected with delegation; captain seat stays human (amends ADR-040/085/089/097)

## Context

Some repository owners want a project to run autonomously and agent-driven, within boundaries such as a budget: react to issues and work items, sequence work, command the crew. The question was asked as an "agent captain". Three rounds of independent review (Fable and Codex, 2026-10-14) rejected an agent-held captain seat and shaped this design.

**What exists:**
- **ADR-083.** No agent or delegated identity can hold the captain seat (`captain_agent_identity_refused`). Delegation can only be lowered, never raised (`delegation_agent_identity_refused`). Captain changes are fenced with a transaction-scoped advisory lock.
- **ADR-040.** It defines the operator agent, acting within delegated bounds under `operator-agent/<role-spec-version>`, fail-closed, paging a human on `must_page_human`.
- **Delegation resolution.** It resolves workflow tier → explicit actions → campaign override → escalation ceiling (`ClampResolvedMatrix`), then `DerivedOperatorAgent`. `delegation.Evaluate` is run-scoped. `validateAutonomy` rejects `mode: auto` on any class without a registered condition. v2 autonomy deliberately keeps automatic backlog ordering and milestone sequencing non-delegable.
- **ADR-085.** It already records a blind shadow (`delegation_shadow_evaluated`) with fixed raise thresholds.
- **ADR-089 and ADR-097.** They give the first officer a read-only, recommend-only seat:
  - attaching it is a captain act (ADR-089 D1);
  - it is bound to one campaign or run;
  - only the captain changes its triggers (ADR-097 D5);
  - operator seats may request specialist jobs (ADR-097 D2).
- **ADR-099 and BRAND §3.** They reserve "captain" for the human.

**What is missing (found by the reviews):**
- **Identity.** `server/actor.go` `isAgentSubject` recognises only `operator-agent/` and `mcp:run:`, and an agent using a human token is indistinguishable from that human.
- **Spend.** The operator is an external session that fishhawkd cannot meter, and budget admission fails OPEN on a sum error.
- **Intake.** The webhook dispatches on `issues.labeled fishhawk` or `/fishhawk run` from any non-bot sender, and the campaign driver starts and retries runs as `operator-agent/campaign`.
- **Injection evals.** The live arms have never run (#3187).
- **Evidence.** Results are not runner-attested (#4363).
- **The seat-mint path** for operator and first-officer seats does not exist yet (#4148).

What is genuinely new is **intake authority** plus sustained, unattended operation, bounded so that it can never widen existing delegation and never narrows what humans may do.

## Options

- **A. An agent-held captain seat with a human owner above it.** Rejected.
- **B. Status quo.** Owners cannot have unattended operation.
- **C. A `charter` AUTHORIZATION MODE (RECOMMENDED).** The human captain issues an expiring, fenced lease that is intersected with existing delegation. A deterministic engine evaluates every concrete action on every path. A product-spawned operator holds only the lease credential, with lease-scoped oversight that cannot be silenced, on one host and one repository.

## Recommendation

Option C.

**D1 Authority model and enforcement contract.**
- **Chain.** Human captain (ADR-083, unchanged) → charter engine → product-spawned operator (ADR-040) → crew. A lease-scoped first officer oversees (D8).
- **Charter is not a tier.** It is an authorization mode INTERSECTED with the workflow's resolved delegation. It is applied AFTER `ClampResolvedMatrix`, and the `OperatorAgent` is re-derived from the intersected matrix, so every downstream knob reader sees it. It can only narrow, never widen, and it never changes what a human may do. The closed charter grant registry does not close workflow-v2's open action namespace.
- **The engine has two arms.**
  - **Run-scoped:** `delegation.Evaluate` plus a lease predicate.
  - **Repository-scoped, new:** it serves the intake class, which precedes any run.
- **Predicates.** They are versioned and evaluate authenticated, server-derived inputs and the exact action parameters. An unknown predicate DENIES. A lease read error is `unevaluable` and delegates nothing. The new classes' conditions are UNMET with no live lease, so a stray workflow declaration delegates nothing without one.
- **Every charter-driven mutation and dispatch path enforces it:** `checkDelegation`, `AutoDriveRunGate`, the campaign driver (`StartRunForCampaignIssue` and its retries), webhook dispatch and the HTTP verbs. Each serializes authorization, spend reservation and durable action intent against revocation.
- **Lease provenance.** Every charter-owned campaign item, run, queued intent and descendant persists the lease ID and generation. After expiry, suspension or revocation, these are enforced on every internal and external dispatch and NEVER fall back to ordinary campaign or webhook authority. Unadmitted items stay held until a human disposes of them.
- **Forbidden under charter:** policy overrides, control-plane edits and indirect gate modification.
- **No agent** holds, offers, claims or relinquishes the seat, or confirms or raises delegation.

**D2 The charter: a validated spec surface and new classes.**
- **Location.** `.fishhawk/operator-charter.yaml` (schema `operator-charter-v0`), human-authored and resolved from the CANONICAL BASE ref. Every implement stage forbids it as a write path, and a CHARTER-DRIVEN merge is refused if its PR diff edits the charter or a workflow spec. Human PRs are unaffected.
- **Contents.** It declares a closed grant list, intake principals (D6), ceilings (D7), the lease duration and the audit parameters (D4/D8), ONE repository and ONE registered host.
- **New classes.** Each registers exactly one backend-evaluable condition. They default to `gated` and are never part of any tier preset, so a workflow must declare them explicitly (e.g. `intake_start_run: {mode: auto, when: charter_principal_eligible}`).
  - `intake_start_run`, with `when: charter_principal_eligible`.
  - `sequence`, with `when: charter_queue_order`. It may only dispatch ALREADY-AUTHORIZED items, in the order of a human-approved dependency graph and a deterministic ordering rule. It can never change priorities, dependencies, membership or milestone scope. The human-faced analog that the shadow stamps is the operator's choice of next item at `start_campaign_item_run`. `sequence` is defined here but is NOT in the pilot (D9).
- **Existing classes** keep their v2 semantics and conditions.

**D3 Non-delegable list and forced paging.** No charter can grant these, and the engine refuses them for the charter identity:
- first-officer triggers or oversight budget (ADR-097 D5);
- delegation confirm or raise;
- every captain verb;
- `budget_override` and `applies_to_override`;
- waive, merge or approve on escalated paths, or with an open security concern (including `diff_secrets` and `permission_drift`);
- evidence exceptions: baseline pin or re-pin, waiving a deterministic gate, accepting a refused measurement;
- automatic backlog ordering and milestone sequencing (v2's existing non-delegable classes);
- deploy, release, tag, force-push;
- `relinquish`;
- edits to the charter, workflow spec, autonomy, `forbidden_paths` or gates;
- host actions;
- `rerun_review` (ADR-095 D2).

**Forced paging.**
- **The page list.** Under charter it is `expandTier`'s high-tier list plus `clarification_request` and `crew_escalation`, whatever `page_human_on` declares. `crew_escalation` is a new enum value, honoured by `Evaluate`'s not-delegable override.
- **`advisory_reviewer_reject` is deliberately excluded.** It does not page on its own, and it is the event that `fixup` / `convergent_concerns` routes.
- **Routing.** Every page event and every ADR-089 D4 blocking finding routes to the D8 inbox and HOLDS.

**D4 The lease lifecycle.**
- **Issuing.** The sitting human captain issues a lease bound to the charter's `content_hash`: 7 days by default, 30 at most, with no auto-renewal. Issuance REFUSES if any D8 audit parameter is missing.
- **Fencing.** Issuance and action admission serialize with captain changes, under the captain store's advisory-lock fencing.
- **Voiding.** Expiry, revocation, a canonical-base charter change or a seat change voids the lease. Voiding invalidates the lease credential, cancels queued charter work, and leaves held items for human disposition.
- **Drain.** It is the default for revocation and expiry: only the execution unit ALREADY DISPATCHED may finish, within its reserved cost, deadline and explicitly authorized effects. Queued reservations confer no execution authority. Every successor dispatch and external mutation revalidates the lease generation through the engine, and an outage denies it. Result recording remains permitted. A revoke may instead cancel active work explicitly.
- **Kill switch, a HARD STOP.** It is a host-local switch checked by fishhawkd's engine, by the runner at dispatch and by the MCP server, and any one of them halts. In-flight charter dispatches are cancelled and outbound mutation is fenced. The fencing supervisor is the seat spawner (#4148).
- **Fail closed.** Every charter action requires a LIVE, verifiable lease at decision time.
- **Scope.** The pilot binds one registered host, and multi-host (ADR-075) is excluded. Lease suspension needs the daemon. The captain owns incident response and audited resumption; the first officer recommends.

**D5 Identity and accountability.**
- **Credentials.** Charter actions run under a dedicated per-lease credential (`operator-charter/<repo>/<lease-id>`). The operator is minted from the seat-mint infrastructure (#4148) with a DISTINCT charter-operator scope profile.
- **Classified as an agent first.** BEFORE any lease can be issued, the subject is registered in `isAgentSubject` / `actorKindForSubject` and refused by:
  - `captain.guardActor`;
  - `delegationconfirm.GuardActor`;
  - ADR-095 D2's re-run verb;
  - every authorization, quorum and audit consumer that refuses agents.

  A test pins this.
- **Specialist jobs.** The specialist-job requester classifies the subject as an operator seat, `actor_kind=agent`, and admits it, per ADR-097 D2's "either seat may request".
- **Product-spawned operator.** The charter operator is product-spawned, holds ONLY the lease credential, and has no access to human credentials or to unrestricted forge or host mutation. A session carrying a human token is not a charter session.
- **Attribution.** Actions are attributed to the charter subject, with the issuing captain recorded as the accountable principal. Commits keep the existing sign-off rules.

**D6 Intake authority.**
- **Eligibility** for `intake_start_run` requires both of:
  - immutable forge ACCOUNT IDs, not logins, of a charter-named principal, with membership current at decision time;
  - an attested principal approval of the exact issue-content DIGEST, given by a label a principal applies and verified through the `issues.labeled` sender.
- **Origin is recorded separately from the approver.**
  - Known agent-originated items are ineligible, and so are agent filings through a human credential that are recorded as agent-originated. Authorship is the principal on a filing approval (for `draft_epic` children and upkeep items), otherwise the forge author.
  - Items of unknown provenance go to captain review.
  - Forge authorship or label application alone never attests human authorship.
- **What voids eligibility.** Any content change after the approved digest voids it until it is re-approved, and so does `issues.transferred`. Comments, reactions, links and tool output can never authorize.
- **Human intake is unchanged.** A `/fishhawk run` or `issues.labeled fishhawk` from a non-principal human starts a run as today, marked `charter_actionable: false`, which queues it for the captain. Only CHARTER-DRIVEN starts are refused, with `charter_intake_refused`.
- **Quarantine** applies to every rendered item.

**D7 Spend and rate.**
- **Controlled invocation.** Live charter operation requires a controlled invocation path: the product-spawned operator, with an enforceable per-call maximum and authoritative usage reconciliation through a signed usage manifest. A session without this is ineligible.
- **Reservations.** An atomic reservation against the lease ceiling is required before each run and each operator call.
- **Unknown usage.** `known_usage=false` or an unknown model keeps the full reservation, charged at the stage's `limit_usd` reserve.
- **No double counting.** Run allocations and call debits do not double-count.
- **Fail closed.** Accounting errors fail CLOSED, and there is no override. Existing tripwires supplement reservations and do not replace them.
- **Aggregate and rate limits.** Ceilings aggregate across every agent acting for the lease, with per-day run and merge rate limits. Reaching any ceiling suspends the lease and pages the captain.
- **Residual.** The ledger is an estimate.

**D8 Oversight.**
- **A lease-scoped first-officer seat.** Issuing a lease attaches it (amending ADR-089 D1 from campaign- or run-scoped, for charter only). The seat covers intake and all descendant campaigns and runs. It retires when the lease voids and the drain completes. Its budget is a reserved fraction of the lease ceiling, and it runs on a model family different from the operator's.
- **When charter actions HALT:**
  - the seat retires;
  - its budget is exhausted;
  - its watch health is staler than the charter's `watch_staleness`;
  - a required job is overdue;
  - a standing trigger is skipped.

  This explicitly overrides ADR-089 D6's and ADR-097 D6's budget-degradation clauses, for charter only. Judgment-only findings remain advisory.
- **Sampling audit.** The CAPTAIN performs it. The engine selects the sample, and the selection is immutable:
  - every hold, reject and intake refusal;
  - plus 10% of the rest;
  - at least 5 per period, or all eligible decisions when fewer exist.

  Each review is agree or disagree with a reason. The charter must declare `sampling_period`, `disagreement_window`, `ack_deadline` and `watch_staleness`. Two disagreements in a window, or a missed deadline, suspend the lease. Recovery is captain-only.
- **Escalation inbox.** An unanswered escalation HOLDS and never auto-approves. Watch the audit metrics for rubber-stamping.

**D9 Earning delegation (ADR-085).**
- **Shadow measurement.** The blind shadow is ADR-085's `delegation_shadow_evaluated`, extended to `intake_start_run` and `sequence`. Raise thresholds are ADR-085 rule 5: a Wilson 95% lower bound of at least 0.9, at least N = 30 condition-met cases, at least 28 days, a 30-day cooldown, and a reset on any doctrine change, stratified per rule 7. Decision types humans never faced remain undelegated.
- **Pilot.** One repository, one host. Classes:
  - `intake_start_run`;
  - `retry`;
  - `fixup` routing;
  - plan `approve` under ADR-040's existing `clean_dual_approval` condition, on non-escalated paths.

  `sequence` and `merge` stay gated in the pilot.
- **Plan approval under an incomplete reviewer round.** A missing review never becomes an approval. An incomplete round (fewer verdicts than configured reviewers, e.g. a provider out of quota) leaves `clean_dual_approval` unmet and queues to the inbox. The pilot reports condition-met counts per class, and an arm with N < 30 is reported as `insufficient evidence`.
- **Intersection.** A delegated approval row never advances a gate that carries an `approvals` block (ADR-083 rule 1). So in a repository whose plan gate requires human approvals, as this one's does, the plan-approve arm is SHADOW-ONLY until a human edits that gate.
- **Expansion** is a captain act on measured results.

**D10 Prerequisites.** These block any lease, in shadow or pilot:
1. Agent classification of the charter subject (D5).
2. The seat-mint infrastructure (#4148), with the charter-operator scope profile and the lease-scoped first officer.
3. The signed usage manifest and fail-closed reservations (D7).
4. #4363, runner-attested evidence.
5. #3187: the live injection arms run, with pre-registered thresholds and coverage of the intake channels.
6. Revocation, drain and the kill switch, tested end to end against queued and running work and a degraded fishhawkd.
7. Intake provenance (D6) and lease-provenance enforcement (D1).
8. The oversight halt and the sampling audit (D8), with standing-watch enforcement operational.

**Ratification questions**
- **Q1:** Keep the captain seat human-only, with `charter` as an authorization mode intersected after `ClampResolvedMatrix` (never widening, never narrowing human authority), enforced by a two-arm deterministic engine on every charter path, with persistent lease provenance?
- **Q2:** Should the charter be a canonical-base surface with `intake_start_run` and a bounded, gated `sequence`, the D3 non-delegable list, and forced high-tier-plus paging that excludes `advisory_reviewer_reject`?
- **Q3:** Should the lease be fenced and expiring (7 days by default, 30 at most), with drain defined as the dispatched unit only, a hard-stop kill switch, and single-host scope?
- **Q4:** Should the charter subject be classified as an agent before issuance, admitted to specialist jobs as an operator seat, through a product-spawned operator holding only the lease credential?
- **Q5:** Should intake bind account IDs and an attested content digest, record origin separately, send unknown provenance to the captain, and leave human intake unchanged (`charter_actionable: false`)?
- **Q6:** Should spend use controlled invocation and atomic reservations, failing closed, with no override?
- **Q7:** Should oversight be a lease-scoped first officer that halts the charter, with a captain-performed stratified audit and required parameters?
- **Q8:** Should delegation be earned through ADR-085's shadow and thresholds, with a pilot of intake, retry, fix-up routing and plan approve under `clean_dual_approval` (shadow-only where the gate requires human approvals), `sequence` and merge gated, and the D10 prerequisites as blockers?

## Decision

**Accepted (2026-10-14), ratified by the Captain as recommended**, after three rounds of independent review (Fable and Codex). Adopt Option C with D1–D10 as written. On the pilot question, the Captain chose option (a): `sequence` is defined but gated, and not in the pilot.

- **Q1: yes.** The charter is an intersected authorization mode, enforced by a two-arm engine on every charter path, with persistent lease provenance. The captain seat stays human-only.
- **Q2: yes.** A canonical-base charter, with `intake_start_run` and a bounded, gated `sequence`, the D3 list, and forced high-tier-plus paging.
- **Q3: yes.** A fenced, expiring lease. Drain lets only the dispatched unit finish, the kill switch is a hard stop, and scope is one host.
- **Q4: yes.** Agent classification before issuance, admission to specialist jobs as an operator seat, and a product-spawned operator holding only the lease credential.
- **Q5: yes.** Account-ID and attested-digest intake, unknown provenance to the captain, and human intake unchanged.
- **Q6: yes.** Controlled invocation and fail-closed reservations, with no override.
- **Q7: yes.** A lease-scoped first officer that halts the charter, and the captain's stratified audit with required parameters.
- **Q8: yes.** ADR-085's shadow and thresholds. The pilot covers intake, retry, fix-up routing and plan approve, which is shadow-only where the gate requires human approvals. `sequence` and merge stay gated, and the D10 prerequisites block any lease.

## Consequences

- **Positioning.** Autonomy comes from governance (leases, a deterministic engine, oversight that cannot be silenced), not from an AI holding authority. That is consistent with BRAND §3 and ADR-099. It is opt-in per repository, and never the default.
- **What owners get.** Unattended intake and recovery within a budget, on one repository and one host, with a human accountable through the lease and the audit. Everything else waits in the captain's inbox, which is honestly a batch-approval queue for the classes not delegated.
- **Schema and code.**
  - The `operator-charter-v0` schema with sync and docs.
  - The two classes, their conditions and the `crew_escalation` page event.
  - The repository-scoped engine arm and the hooks on every charter path.
  - Lease storage with fencing, provenance stamping, and verbs to issue, revoke and read a lease.
  - The charter credential and its agent classification.
  - The product-spawned operator with a usage manifest.
  - Reservations.
  - Intake digest attestation.
  - The lease-scoped first officer, the oversight halt and the audit.
  - Registered audit categories.
- **Observability.**
  - A charter status surface (`GET /v0/charter`).
  - The gate view.
  - A `restart-blockers` item for queued charter dispatches.
- **Residuals.**
  - Sampling is retrospective, so the non-delegable list and gating carry the real safety load.
  - A different model is not independent judgment.
  - Forge account identity is trusted.
  - The spend ledger is an estimate.
  - A per-host halt does not suspend the lease without the daemon.
- **Implementation:**
  1. The D10 prerequisites, each its own issue.
  2. The schema and conditions.
  3. The engine arms, leases, fencing and provenance.
  4. Intake attestation and routing.
  5. Spend reservations.
  6. The lease-scoped oversight and audit.
  7. The ADR-085 shadow extension.
  8. The pilot, a captain act.

## Relations

- **ADR-040:** amended. It adds the charter mode, the two classes and conditions, and the product-spawned charter operator.
- **ADR-085:** amended. Charter is its first enablement path, which rule 1 ("recommendations only") now expressly permits under charter; the shadow is extended to the two classes.
- **ADR-089:** amended for charter only. A lease-scoped seat attached by the lease (D1), and the charter overrides D6's budget-degradation clause.
- **ADR-097:** amended for charter only. It overrides D6's degradation clause; D5 is reaffirmed as non-delegable; D2's operator-seat specialist-job requests apply to the charter operator.
- **Unchanged:** ADR-083 and ADR-099.
- **Related:** ADR-095 D2, ADR-075 (multi-host, excluded), #4148, #4363, #3187, #4330, ADR-098 and ADR-100 (shadow-first precedent).
