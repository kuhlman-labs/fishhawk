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

## Amendment A3: the dispatcher's caps once the server sizes the host, and stale text

Ratification: Captain, 2026-11-27 (issue #5610), ratifying the text exactly as converged in the operator's ADR review (docs/adr/README.md § "Drafting and review"; round 3: Fable claude-fable-5-2 and Astra gpt-6-astra both APPROVE, at repo commit d991330ad). Recorded by the operator. It changes no front-matter field.

**Context.** Q6 gave the dispatcher its own caps because "until #4241 lands, `FISHHAWK_HOST_CAPACITY` is read by no code". A1 item 1 kept max in-flight (default 2) and the per-tick cap "until #4241 replaces them with host-declared capacity". #4241 has landed. ADR-094 (#4239) Amendment A5 makes this dispatcher's heartbeat the host's one capacity declaration. Three things remain:
- what the dispatcher's caps now mean;
- the coupling between the tick and the server's queue TTL. Today the interval cap is a separate 45 s constant (`backend/internal/mcpserver/host_dispatcher_loop.go:52`; `backend/cmd/fishhawk-mcp/host_dispatch.go:32`), and queued re-POSTs spend the per-tick budget (`host_dispatcher_loop.go:1076-1092`);
- two stale lines in this record.

**Decision.**
1. **`--max-in-flight` becomes an optional dispatcher share.**
   - **Unset, the new default.** The share is the governing host capacity, with no separate bound: every stage the dispatcher spawns holds one slot of `host:<label>:capacity` (ADR-094 A2 item 2), so the governing capacity of ADR-094 Amendment A5 bounds it. An operator who wants headroom for interactive sessions sets the share explicitly.
   - **Set.** It bounds the stages in this dispatcher's own ledger, dispatched or running (`host_dispatcher_loop.go:1326-1333`). It is still checked before the marker POST (`:1311-1314`). At its share the dispatcher POSTs no marker, so it creates no queue row and no capacity reservation, and its queued stages lapse after `QueueTTL`. A share at or above governing capacity adds no tighter bound when the ledger matches live slot holders and capacity is stable; retained ledger entries can still make it bind.
   - **What this replaces.** It replaces Q6's "max in-flight spawns (default 2)" and, for max in-flight, A1 item 1's "remain until #4241 replaces them". The flag, `FISHHAWK_HOST_DISPATCH_MAX_IN_FLIGHT` and the `cap_reached:in_flight` refusal keep their syntax.
2. **The heartbeat carries the host declaration.**
   - The dispatcher sends `FISHHAWK_HOST_CAPACITY` as the heartbeat's `host_capacity`. It sends a new optional `FISHHAWK_HOST_IMPLEMENT_LIMIT` as `host_implement_limit`. Both follow ADR-094 A2 item 7's parse rules, and an invalid value refuses dispatcher start-up.
   - An unset variable omits its field, which declares nothing for that limit; the other field still governs (ADR-094 Amendment A5 item 1).
   - At ingest, a value outside 1..64 drops the declaration, never the heartbeat (ADR-094 Amendment A5 item 1). The rest is stored, so liveness and status still publish, and the response's `dropped_declaration` names the field. The dispatcher logs it once per distinct value, as A2 item 5 logs `dropped_refusals`, and the doctor rung reports it. No earlier declaration is carried forward.
   - Before its first marker POST after process start, a dispatcher with either declaration field configured needs one successful heartbeat acknowledgement that retained its declaration. Until then it keeps publishing status and retries each tick, but spawns nothing. Later heartbeat failures follow ADR-094 Amendment A5's freshness and fallback rules. The dispatcher publishes this first heartbeat before its first candidate pass and waits for the acknowledgement within the heartbeat timeout, so a restart does not add an interval to the refresh gap. This narrows A2 item 1 at startup.
   - `scripts/dev` passes `FISHHAWK_HOST_IMPLEMENT_LIMIT` through beside `FISHHAWK_HOST_CAPACITY` (`scripts/dev:3036-3047`).
   - The dispatcher's markers keep sending `capacity`, which ADR-094 Amendment A5 ignores for admission.
   - A2 item 7 ("no gate reads it") is narrowed: admission reads these two fields under ADR-094 Amendment A5. The rest of the heartbeat stays advisory, and no dispatch, approval or merge authority changes.
   - Q6's "Its env carries `FISHHAWK_HOST_CAPACITY` once the marker reads it" now means this declaration.
3. **Queued refreshes are exempt from the per-tick cap.** On each successful candidate pass, eligible queued stages are offered a refresh without spending that cap. FIFO position is retained only when consecutive successful marker refreshes are less than `QueueTTL` apart (`host_dispatcher_loop.go:38-45`; `backend/internal/concurrency/concurrency.go:61-64`). Pause, share, daily-cap, authorization and eligibility checks still apply; failures and long ticks can let rows lapse. The interval and the cap follow from that rule:
   - **Interval.** The interval bound is `QueueTTL / 2`, derived from `concurrency.QueueTTL` or from a copy pinned to it by a parity test. That is 30 s today. The default equals the bound, so today's 30 s default is unchanged. A value above the bound is refused as today. The 45 s constant is retired: it is three-quarters of `QueueTTL`, not half. The half-TTL interval provides scheduling margin; it does not bound tick duration or guarantee refresh before expiry.
   - **Per-tick cap.** A re-POST that refreshes a stage the dispatcher already holds queued (rank 0, `:1276-1285`) does not spend the per-tick cap. The cap (default 4) bounds only new candidates, those this dispatcher process has not recorded as queued; after a restart every candidate is new for one tick. A refresh creates no new reservation, and skipping one forfeits the stage's FIFO position after `QueueTTL`, so a refresh never depends on the cap's value. An admitted refresh spawns without spending the per-tick cap. Governing capacity bounds simultaneous live holders, and an explicit share bounds this dispatcher's ledger occupancy; capacity does not bound the total number of spawns during a tick. The per-tick cap stays with this meaning; this replaces A1 item 1's "remain until #4241 replaces them" for the per-tick cap.
   - **By design, not violations.** Any refusal before the marker POSTs nothing: pause (Q5(b)), the share (item 1), the daily cap, and campaign, budget or hold refusals. A tick that fails before the candidate pass also re-POSTs nothing, as today.
4. **`--daily-cap` is unchanged.** It stays an opt-in spawn-count guard (A1 item 2). It is not a spend guard: A1's Context says it counts spawns and weighs no stage's cost. A spend backstop remains the separate decision A1's Consequences name.
5. **Clarifying lines.**
   - **Shared preview (Consequences).** "serialized by `local-acceptance` limit 1" now reads: serialized by the built-in `host:<label>:preview-target` group at limit 1. A spec adds that group to acceptance's default `{capacity}` set (ADR-094 D5 as amended by A2 items 1 and 7). This repository declares it at `.fishhawk/workflows.yaml:828-831`. Without that declaration, acceptance holds only capacity (`backend/internal/concurrency/README.md:302`).
   - **Campaign `max_in_flight` (Q6).** "It honours a campaign's `max_in_flight`" names a field that does not exist: the campaign model carries no such cap (`backend/internal/campaigndriver/driver.go:81-87`). A campaign's work in progress is bounded by the campaign driver's `DefaultMaxParallel` (4 running items per campaign; `driver.go:87`, `:1183`). The dispatcher honours campaign state through Q3 (running campaigns only). This amendment adds no field.

**Consequences.**
- **Upgrade order.** Upgrade fishhawkd first. An older fishhawkd refuses a heartbeat that carries the new fields (`backend/internal/server/host_dispatcher_heartbeat.go:60`), and campaign status then reads `host_dispatcher_absent`. A dispatcher with a declaration configured also spawns nothing until fishhawkd is upgraded (item 2).
- **Rollback.** Revert the dispatcher binary. The old default of 2, the 45 s bound and the per-tick charging return.
- **Behaviour change.** A host whose `.env` sets no share now lets unattended work take the whole governing capacity, and later interactive dispatches queue behind it in FIFO order.
- **Docs updated in the implementing change:**
  - `.env.example:102-112`;
  - the `scripts/dev` hint (`:3129`);
  - `backend/cmd/fishhawk-mcp/README.md:146-148`;
  - `scripts/README.md` § "Host dispatcher";
  - `docs/api/v0.openapi.yaml` for the heartbeat fields.
- **Unchanged.** Q1–Q5, Q7 and A1 items 2–3 stand unchanged. A2 items 1–6 otherwise stand, subject to item 2's startup publication prerequisite.

**Risks and residuals.**
- **Tick duration.** Uncharged refreshes make a tick grow with the dispatcher's queued stages, bounded by eligible candidates and the campaign driver's cap; a long tick can let a row lapse.
- **Startup prerequisite.** An unreachable heartbeat endpoint at start stops a configured dispatcher from spawning. This fails closed and is visible in `status.json` and the doctor. A restart that outlasts `QueueTTL` between refreshes still forfeits the queued stages' positions.
- **A share is per dispatcher.** It does not see session-spawned stages. When ledger reads fail, it keeps entries and stays closed (`host_dispatcher_loop.go:1213-1215`).
- **Not decided here.** The work-in-progress caps are not decided here: the ADR-041 decomposition fallback, the `fishhawk_run_children` clamp and a campaign `max_parallel` field. Neither are mirrored constants beyond the interval bound, or the racing-spawners residual.

## Amendment A4: the dispatcher declares host group limits

Ratification: Captain, 2026-11-28 (issue #5659), ratifying the text exactly as converged in the operator's ADR review (docs/adr/README.md § "Drafting and review"; round 3: Fable claude-fable-5-2 and Astra gpt-6-astra both APPROVE, at repo commit bf8cf6403). Recorded by the operator. It changes no front-matter field.

**Context.** A3 item 2 makes this dispatcher's heartbeat carry the host's capacity and implement-limit declaration (ADR-094 (#4239) Amendment A5). ADR-094 (#4239) Amendment A6 adds a third, optional part, `host_group_limits`, which sizes every other host-scoped group, the host verify group and the shared preview target among them. This amendment gives the dispatcher its input and states how it sends it. A3 item 2's rules for parsing, start-up refusal, the ingest drop and the startup acknowledgement apply to the new field unchanged.

**Decision.**
1. **Input.** The dispatcher reads an optional `FISHHAWK_HOST_GROUP_LIMITS`, a comma-separated list of `<name>=<n>` entries. For example, `verify=2` gives the host verify group two slots. Whitespace around entries is ignored.
   - Each name follows ADR-094 A6 item 1's key rules, and each value follows ADR-094 A2 item 7's integer rules (1..64).
   - A duplicate name, a reserved name (`capacity`, `implement`), an invalid group name, an invalid value or more than 16 entries refuses dispatcher start-up and names the entry, as an invalid `FISHHAWK_HOST_CAPACITY` does under A3 item 2.
   - If the variable is unset, the field is omitted, which declares nothing for any host group. A set but empty value refuses dispatcher start-up, as ADR-094 A2 item 7 states for `FISHHAWK_HOST_CAPACITY`. `scripts/dev` passes no empty variable through (`scripts/dev:3058-3063`).
2. **Sending.** The dispatcher sends the parsed list as the heartbeat's `host_group_limits` object, beside `host_capacity` and `host_implement_limit`.
   - A3 item 2's startup prerequisite counts this field: a dispatcher with any of the three configured waits for one successful acknowledgement that retained its declaration before its first marker POST.
   - A dropped declaration at ingest is logged once per distinct value and reported by the doctor rung, as A3 item 2 states.
3. **Passthrough.** `scripts/dev` passes `FISHHAWK_HOST_GROUP_LIMITS` through to the dispatcher beside `FISHHAWK_HOST_CAPACITY` (`scripts/dev:3036-3047`). It also sets fishhawkd's `FISHHAWKD_DEFAULT_HOST_GROUP_LIMITS` from the same `.env` value (ADR-094 A6 item 2 (b)).
4. **Shared preview.** A3 item 5's "serialized by the built-in `host:<label>:preview-target` group at limit 1" now reads: serialized by that group at its governing limit under ADR-094 A6 item 2, with a built-in fallback of 1. A host with one shared preview target keeps 1. A higher value needs the operator to provide enough independent preview targets for the stages it admits.
5. **Unchanged.** These stand:
   - the dispatcher's share (A3 item 1);
   - the per-tick cap and the queued-refresh exemption (A3 item 3);
   - the daily cap (A3 item 4).

   The dispatcher does not count its own spawns against any host group limit; the server's admission does.

**Consequences.**
- **Upgrade order and rollback** are A3's: upgrade fishhawkd first, and revert the dispatcher first. An older fishhawkd refuses a heartbeat that carries `host_group_limits` (`backend/internal/server/host_dispatcher_heartbeat.go:60`), so the dispatcher sends the field only when the variable is set.
- **Docs updated in the implementing change:**
  - `.env.example`;
  - the `scripts/dev` passthrough comment;
  - `backend/cmd/fishhawk-mcp/README.md`;
  - `scripts/README.md` § "Host dispatcher".

**Risks and residuals.**
- **Session-spawned stages.** Stages spawned outside the dispatcher are admitted under the same server-resolved limits, but they do not declare them.
- **No fresh declaration.** When a group has no fresh declaration entry, admission uses the fishhawkd default or the built-in fallback (ADR-094 A6 item 2). A stopped dispatcher's last declaration keeps governing until it goes stale, which can take up to 15 minutes (ADR-094 A5's freshness ceiling).
