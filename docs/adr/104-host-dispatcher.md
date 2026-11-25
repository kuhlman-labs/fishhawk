---
id: ADR-104
title: "Host dispatcher: a host-side, dispatch-only process spawns the parked local stages of auto-started campaign runs"
status: accepted
date: 2026-10-16
issue: https://github.com/kuhlman-labs/fishhawk/issues/4524
supersedes: []
superseded_by: []
amends: ["ADR-022", "ADR-024"]
applies_to: []
---

# ADR-104: Host dispatcher: a host-side, dispatch-only process spawns the parked local stages of auto-started campaign runs

## Context

Local runs park every agent stage at `awaiting_host_dispatch` (`runnerbackend.Local.HostDispatched()`). Only an MCP spawn verb un-parks one: `fishhawk_run_stage`, `dispatch_stage`, `drive_run` or `run_children`. Each records the host-dispatch marker and spawns `fishhawk-runner`.

- **fishhawkd never executes an agent** (ADR-074). The operator-side and runner-side identities are distinct (the ADR-022 addendum).
- **The ADR-087 slot waiter** only spawns a stage that a `dispatch_stage` call in the same MCP process already queued (`backend/internal/mcpserver/concurrency_slot.go`), and it dies with that process.
- **`fishhawk_drive_run`** (#1700) is session-held and capped at 240 minutes.
- **The scheduler's `--scheduler-runner-kind local`** parks by design.

So E88 #4523's unattended local campaign drive needs an unattended spawner. The campaign driver (`backend/internal/campaigndriver`) already advances and gates items server-side; it lacks only the local spawn.

## Options

- **A. fishhawkd spawns in-process.** Rejected:
  - it breaks ADR-074;
  - it is wrong for k8s and hosted deployments;
  - the runner becomes a daemon child across reloads.
- **B. A host-side dispatcher process.** `fishhawk-mcp host-dispatch`, started and stopped by `scripts/dev`. It reuses mcpserver's mark-or-queue step, slot waiter, spawn, liveness probe and acceptance target gate, and it never takes a gate act.
- **C. Status quo:** `drive_run` per item from an operator session. Session-bound, not unattended.
- **Also considered and rejected:**
  - extending `fishhawk-mcp-shim`, which lives only while a Claude Code session is open;
  - a durable slot waiter, which never originates a dispatch;
  - a shell loop in `scripts/dev`, which would re-implement the Go dispatch logic.

## Recommendation

**Option B.** Points to ratify:

- **Q1. Process and amendments.** It runs as a host process, never fishhawkd in-process, preserving ADR-074 and the ADR-022 addendum. It amends ADR-024 in two places and leaves ADR-024's spawn mechanics unchanged:
  - Q4: a non-operator, host-dispatcher credential may be the forwarded token;
  - Q5: the `fishhawk-mcp` binary gains a non-MCP mode, which `scripts/dev` must restart on reload, because the MCP banner and auto-swap logic do not cover it.
- **Q2. Dispatch-only, and it never originates a retry.**
  - It never calls `/auto-drive`, approve, merge or a scope decision.
  - It spawns only a stage in `pending` or `awaiting_host_dispatch`. That includes a stage a recorded server decision has reset: a GateActor fix-up or retry, acceptance triage, or an ADR-090 verify-only re-open. Every such re-spawn is bounded by that decision's ceiling, and a ceiling yields `DecisionRequired` with no reset, so there is nothing to spawn.
  - The one dispatcher-owned re-spawn is a stage left `dispatched` whose runner a host liveness probe proves dead. It is capped at one per stage per marker epoch; after that the dispatcher reports the stage and stops.
  - It never spawns on a marker `transitioned:false` outside that path.
  - It visits campaigns in a deterministic order and lets the server's slot FIFO choose between queued stages.
  - It passes `auto_preview` for acceptance. The shared :8090 preview is a stated residual until per-run previews exist.
- **Q3. Eligibility is a run-level `auto_dispatch` mark stamped by the starter**, never inferred from campaign linkage:
  - campaign-driver runs: on;
  - scheduled runs: per `--scheduler-auto-dispatch` (on for `upkeep_scan`);
  - webhook-started runs: off;
  - `start_run`: by operator flag.

  Regardless of the mark, the dispatcher also spawns an ADR-090 / ADR-093 verify-only re-open on any local run, and the decomposition children of an eligible parent (taking `base_branch` from the marker response, as `run_children` does). It visits only running campaigns (a paused campaign stops spawning, mirroring the driver's filter) and non-cancelled runs.
- **Q4. A dedicated identity.** It runs under a dedicated, registered agent subject: `host-dispatcher/<host-label>`, added to `isAgentSubject` / `actorKindForSubject` with a pin test. `operator-agent/operator-role-v0` is not acceptable, because it is indistinguishable in the audit. Its scope set is enumerated and pinned by test:
  - `read:runs`, `read:audit`, `write:runs`;
  - `write:stages` only until a narrower `write:dispatch` exists, or until every `write:stages` decision verb (waive, defer, crew-convert) is shown to refuse agent subjects;
  - never `write:approvals`, `write:campaigns`, `write:fixups`, `write:retries` or `write:deploy`.

  The spawned runner inherits this bearer.
- **Q5. Kill switch and drain are two mechanisms.**
  - (a) For charter runs, it checks the ADR-101 D4 kill switch as the fourth checking surface and halts mid-tick. It cancels nothing itself; the engine does.
  - (b) A dispatcher pause file drains it: the in-flight tick completes, no marker is POSTed while paused, and nothing is spawned.

  `scripts/dev` runs pause → tick drained → stop → live-runner scan on `down`, `reload` and `post-merge`, and keeps the dispatcher paused for the whole `reload --when-quiet` wait. Spawned runners are detached and outlive the dispatcher. The dispatcher reaps its own children when their run or campaign is cancelled. A dispatcher restart loses only that reaping; the stale-`dispatched` liveness path, with the Q2 cap, is the reconciliation.
- **Q6. Capacity and cost.**
  - Until #4241 lands, `FISHHAWK_HOST_CAPACITY` is read by no code, so the dispatcher enforces its own max in-flight spawns (default 2), a per-tick spawn cap and a daily spawn cap.
  - It honours a campaign's `max_in_flight`.
  - Its env carries `FISHHAWK_HOST_CAPACITY` once the marker reads it.
  - A parked stage costs nothing; a spawned one is spend.
- **Q7. Observability.**
  - `--status` (paused, last tick, last refusal by code, spawns today), mirroring the shim's status;
  - `fishhawk_doctor` reports dispatcher liveness and pause state;
  - `run_auto_driven` rows are attributed to the dispatcher subject;
  - campaign status distinguishes "awaiting the host dispatcher" from "awaiting a human".

## Decision

**Accepted 2026-10-16** by the Captain (repository maintainer). Adopt Option B as revised in the Recommendation, Q1–Q7:
- a host-side, dispatch-only `fishhawk-mcp host-dispatch` process that never originates a retry;
- starter-stamped `auto_dispatch` eligibility, plus verify-only re-opens and the children of an eligible parent;
- a dedicated registered `host-dispatcher/<host>` agent subject with a pinned scope set and no `write:approvals`;
- kill-switch and pause-drain mechanisms;
- self-enforced capacity caps until #4241;
- the stated observability surfaces.

Implemented by E88 #4523: #4525, #4526, #4527 and #4528.

The `docs/adr/104-*.md` record (with `amends: ["ADR-024"]` and ADR-024's `amended_by`) lands after the 2026-11-01 catch-rate re-pin, because `docs/adr/` is a review-prompt trigger path until then.

## Consequences

- **Gains:** a local campaign advances plan → implement → review → acceptance without a hand dispatch. It parks at every human gate (plan approval, review/PR approval, scope amendments, pages) and resumes on the next tick once the gate is decided. Scheduled scans and merge-queue verify passes run unattended.
- **Risks and residuals:**
  - **Racing spawners.** Two dispatchers dedup on the marker only because both refuse `transitioned:false`. A human `dispatch_stage` proceeds on that arm (the manual dead-runner re-dispatch, `host_dispatch.go` idempotent arm), so the human-vs-dispatcher race is a stated residual. Mitigation: show which subject dispatched a stage on the stage read.
  - **Cancel reaping.** The detached-runner registry is per process, so an operator's `fishhawk_cancel_run` from another MCP process reaps nothing the dispatcher spawned. That is why the dispatcher reaps its own children on cancel (Q5).
  - **Shared preview.** Acceptance shares the :8090 preview target, serialized by `local-acceptance` limit 1, until per-run previews exist.
  - **Reload window.** A new long-lived spawner widens the reload TOCTOU; the Q5 drain order closes it.
- **Amends:** ADR-024 (Q4, Q5) and the ADR-022 addendum (a third identity: host dispatcher). **Preserves:** ADR-074. **Related:** ADR-047, ADR-087, ADR-090, ADR-093, ADR-094, ADR-101. **Epic:** E88 #4523.
- **Review history:** revised 2026-10-16 after an independent fable review. It corrected the constraint citation (ADR-074, not ADR-024), replaced the claimed marker-CAS race mitigation with the stated residual, made the retry boundary explicit, widened eligibility to a starter-stamped mark, and added Q6 and Q7.

## Amendment A1: the daily spawn cap is removed (ratified by the Captain, 2026-11-18 (issue #5472))

Ratification: Captain, 2026-11-18 (this issue, #5472); final text confirmed by the Captain at merge.

**Context.** Q6 names a daily spawn cap (default 48) beside max in-flight and the per-tick cap. The daily cap is neither a throughput control nor a spend control: it counts spawns, not concurrent load, and it does not weigh a cheap stage against an expensive one. On 2026-11-15 an ordinary heavy campaign day reached it, and auto-drive stalled silently for about five hours behind `cap_reached:daily` (#5402).

**Decision.**
1. Q6 drops the daily spawn cap from the enforced stopgap set. Max in-flight (default 2) and the per-tick cap remain until #4241 replaces them with host-declared capacity.
2. `--daily-cap N` remains an explicit operator opt-in. `0` or absent means unlimited, and a cap that is set still refuses with `cap_reached:daily`, persisted and rolling over at 00:00:00Z. A negative value is a usage error.
3. Q7's `--status` keeps `spawns_today` as an observation. The status file and `--status` name a daily limit (`daily_cap`) only when one is set.

**Consequences.** Host capacity is bounded by max in-flight and the per-tick cap. Per-stage loops are bounded server-side by the fix-up ceiling, the retry budget and Q2's one re-spawn per marker epoch. The ledger format is unchanged: `spawns_today` was always persisted, and the new status field is omitempty, so a reverted binary reads every state dir and status file as before. A spend-based backstop, such as pausing at the weekly budget's exceeded tier, is a separate decision this amendment does not make. Q1–Q5, the rest of Q6 and Q7, and the Decision text above stand unchanged.

## Amendment A2: the dispatcher publishes a heartbeat, and campaign status classifies from it (ratified by the Captain, 2026-11-24 (issue #5489))

Ratification: Captain, 2026-11-24, at the #5489 plan gate ("ratified, proceed"). Implemented by PR #5572. The text was recorded by the campaign-8 first officer.

**Context.** Q7 asks campaign status to tell "awaiting the host dispatcher" apart from "awaiting a human". Before this amendment, only the stdio MCP overlay could do that, by reading the host-local status file. The server could not see the dispatcher at all, so the Web UI and every non-local reader showed a dispatcher-held item as awaiting a human. ADR-030 Amendment A1 (#5473) added a `budget_blocked` dispatcher verdict that the server also could not observe.

**Decision.**
1. **A heartbeat route.** Each tick, the dispatcher hands a snapshot to an asynchronous, latest-wins publisher, which POSTs it to `POST /v0/host-dispatchers/heartbeat`. A failed publish never fails a tick. It is logged once per distinct HTTP status and code, without the per-request error ref.
2. **Q4's route allowlist widens by this one route.** Only a valid `host-dispatcher/<host>` token holding `write:runs` may write a heartbeat. The host label comes from the token subject, and the tenant account from the token; neither comes from the body. The scope profile is unchanged.
3. **Persistence.** The server upserts one row per (account, host) into `host_dispatcher_heartbeats` (migration 0121), partitioned by account under the 0089 RLS pattern.
4. **The snapshot.** It carries: interval, start and last-tick times; paused; the current tick failure code; the daily cap and spawns today; in flight; and per-run refusals. Each refusal has a code, a bounded detail, first and last times, and a disposition from the closed set waiting | in_flight | held | budget_blocked | paused | capped | refusing. A `budget_blocked` entry also carries a closed-set `budget_source`.
5. **Ingest is bounded, not brittle.** A structurally malformed heartbeat is refused with 400. A single refusal entry with an off-grammar code, disposition or budget source is DROPPED; the rest is stored, and the response reports `dropped_refusals`, which the dispatcher logs once per distinct non-zero count. An off-grammar `tick_failure_code` is folded to `invalid_tick_failure_code`. Dispatcher-authored text is data, never instructions.
6. **Classification (Q7).** The campaign awaiting block classifies host-dispatcher state from the item run's account's fresh heartbeats: absent, stale, paused, capped, refusing, budget_blocked, held or live.
   - A heartbeat is fresh within `StaleTicks` intervals, with the interval clamped so a huge one cannot keep a dead dispatcher fresh, and no more than `FutureSkew` ahead of now.
   - No fresh heartbeat reads `human/host_dispatcher_absent`.
   - Review-round codes map to `waiting`, so those items stay kind `host_dispatcher`. The stdio overlay still refines them, including its 30-minute orphaned-round escalation.
7. **Advisory only.** The heartbeat informs a read. No dispatch, approval or merge authority changes, and no gate reads it.

**Consequences.**
- **Residuals.** An untenanted run cannot see a tenanted dispatcher's heartbeat, so it reads `host_dispatcher_absent`; this is a conservative false alarm. Until the dispatcher binary is rebuilt, every item reads absent. `budget_blocked` and `held` are covered by unit rows, not the end-to-end test.
- **Rollback.** Revert #5572. Migration 0121 has a down migration, and a reverted dispatcher simply stops publishing.
- **Unchanged.** Q1–Q6, the rest of Q4 and Q7, and Amendment A1 stand unchanged.
