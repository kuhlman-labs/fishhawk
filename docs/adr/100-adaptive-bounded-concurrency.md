---
id: ADR-100
title: "Adaptive, bounded concurrency for host-dispatched stages: a product-owned headroom controller beside the integer limit, operator override, staged shadow→canary→enforce (amends ADR-087 limit rule and ADR-094 D4/Q1)"
status: accepted
date: 2026-10-14
issue: https://github.com/kuhlman-labs/fishhawk/issues/4403
supersedes: ["ADR-087", "ADR-094"]
superseded_by: []
applies_to: []
---

# ADR-100: Adaptive, bounded concurrency for host-dispatched stages: a product-owned headroom controller beside the integer limit, operator override, staged shadow→canary→enforce (amends ADR-087 limit rule and ADR-094 D4/Q1)

## Context

**ADR-087** gave host-dispatched stages concurrency groups with a fixed integer `limit`. Admission is `holders < limit`. The limit is resolved server-side from the run's cached spec (`server/host_dispatch.go` `resolveStageConcurrency`), and the store compares against the request's limit (`backend/internal/concurrency/store.go`). ADR-087 says "the ADMITTING stage's limit governs".

**ADR-094** (accepted 2026-10-10, entirely unimplemented: #3969, #4240–#4243 are open) adds three things:
- groups keyed on the resource they protect;
- a host capacity group "declared BY THE HOST", `FISHHAWK_HOST_CAPACITY`, default 3;
- integer slots with no per-stage weights. Q4: "revisit only on a measured campaign showing plan stages wasting capacity".

A fixed number is wrong in both directions.
- **It overloads a stressed host.** On 2026-10-11/12, campaign 24ac68d6 at the hand-tuned `limit: 2` still hit:
  - fseventsd swap exhaustion (load 158 on 14 cores, swap at 99%);
  - a go-build cache that grew about 20 GiB an hour;
  - Docker memory pressure;
  - a category-C acceptance failure from a shared-resource race.
- **It wastes a quiet host.** That host sat idle at `limit: 2` while stages queued.
- **Re-tuning is manual.** The operator re-tunes the number by hand and edits the spec.

The operator asked whether the operator agent could choose the number, declared as `auto`. The #4330 direction applies: probes are product code, a model is used only on exception, and the design is host- and codebase-agnostic. ADR-089 bounds the first officer: D2 withholds `write:stages`/`write:runs`, and D9 says remediation "remain[s] human or driver actions. The seat may only *recommend*".

**What exists today:**
- The only host probe is `runner/internal/hostload`: load1 via `/proc/loadavg`, `sysctl vm.loadavg` or `uptime`, CPU only.
- There is no backend ops monitor (#4142).
- The promote verb is gated on `write:runs`.

## Options

- **A. Status quo.** Fixed limits and an operator-tuned capacity.
- **B. The operator agent sets the limit.** This is non-deterministic, costly and unauditable, and contradicts #4330.
- **C. A product-owned, bounded controller beside the fixed limit, with an operator override, staged enablement (shadow, then canary, then enforce), and explicit amendments to ADR-087/094 (RECOMMENDED).**

## Recommendation

Option C.

**D1 Declaration: a sibling block, so the integer limit stays.**
- **Group limits.** A stage concurrency declaration keeps its integer `limit` and may add `auto: {min, max}`, with `1 ≤ min ≤ limit ≤ max ≤ 64`.
- **Host capacity.** It keeps `FISHHAWK_HOST_CAPACITY=<int>` and may add `FISHHAWK_HOST_CAPACITY_AUTO=<min>-<max>`.
- **Which limit is in force.** In modes `off` and `shadow` the integer `limit` governs, exactly as today, which also makes rollback free. Only `enforce` hands admission to the controller.
- **Validation.** The schema enforces the inequality. A declaration with an unsupported `auto` (wrong group kind, non-host runner) fails closed at spec validation. The existing ADR-094 shorthand-versus-`groups[]` exclusivity is preserved.
- **Old clients.** They never see a type change.
- **Mode switch.** `FISHHAWKD_CONCURRENCY_AUTO=off|shadow|enforce`, default `shadow`. It is daemon-side.

**D2 Group kinds, and where `auto` is allowed.** `kind` is orthogonal to ADR-094 D3 `scope`.

| Kind | What it protects | ADR-094 mapping | `auto` |
|---|---|---|---|
| `host` | host CPU, memory, disk | the capacity group | allowed on ONE group per host (the ADR-087 implement group until #4241 lands, then the capacity group) |
| `quota` | a provider rate or quota | account | refused; keeps ADR-094 D6 fixed limits and parking |
| `exclusive` | a single target | preview target, the `local-verify` / per-base merge-candidate line | refused; its integer `limit` is unchanged |

- **Exclusive does not force `limit: 1`.** It only refuses `auto`, so a repository's named `limit: 4` group keeps working. Overrides can never relax preview exclusion, ADR-092 per-base serialization, re-anchoring, or ADR-090 exact-head verification.
- **Undeclared kinds** default to `exclusive`.
- **Type groups** nested inside capacity stay integers. There is one controller per host, so two never fight.

**D3 Headroom probes are portable and domain-scoped.**
- **What is probed.** The probe reads the execution resource domain: effective cgroup CPU and memory limits and PSI where present, load per core otherwise, swap, container-runtime memory against its cap, and free space on the runner's build-cache and worktree paths. Those paths are discovered from the runner's own environment, not assumed `GOCACHE`.
- **Where probes live.** Per-platform probes (macOS, Linux, Linux-in-container) live with #4330's and are shared with it.
- **Unsupported domains.** A domain the probes don't support reports `not_applicable`, and the limit stays at the integer `limit`. This covers a Kubernetes pod without node telemetry, and CI, where the declaration is already inert for non-host runners.
- **Load as a signal.** Load ≥ 4× cores is the #3663 starvation classifier and lags swap, so it is a trip signal, not an early warning.

**D4 The controller.**
- **Where it runs.** In fishhawkd.
- **Where readings come from.** The MCP process on the dispatching host samples headroom and sends it on the host-dispatch marker and on each waiter poll. The readings are client-supplied, like the host label and capacity (the ADR-094 Q1 residual), and the spec's `max` is the trust bound.
- **State.** Per group key, fishhawkd persists `{limit, generation, last_sample_at, healthy_streak, trip_level, backoff_until, override, override_expires_at}` in Postgres beside the slot table. One fenced writer per key means a restart or a second replica sees one value, and stale reporters are rejected.
- **Authority.** Admission reads the stored effective limit under the same group locks as the holder check and the dispatch CAS, in ADR-087's one transaction on one connection. For an `auto` group, the stored value governs and not the request's limit.
- **Staleness, the safety floor.** The effective limit is `min` whenever the record is older than 3 sample intervals, or 3 consecutive samples are `unknown`. That one rule covers a dead probe, a stuck controller, a clock gap and a restart (a restart resets `healthy_streak`).
- **Sampling.** Every 30 seconds. N = 10.
- **Raise.** By one, only when the group is SATURATED (holders equal the current limit, with a queued waiter) and all N samples are healthy against RECOVERY thresholds that sit below the trip thresholds:

  | Signal | Recovery (healthy) | Trip |
  |---|---|---|
  | swap | < 70% | ≥ 90% |
  | load | < 2× cores | ≥ 4× cores |
  | runtime memory | < 75% | ≥ 90% |
  | disk | above floor + 20% | below the floor |

  Thresholds are versioned probe-profile defaults, overridable per host.
- **Lower.** On a tripped signal, lower by one per interval, down to `min`. After a trip at level L, L is not retried until a back-off expires. The back-off doubles per trip: 5, 10, 20 minutes, capped at 60.
- **Critical pressure.** Swap ≥ 97% or disk exhausted blocks new admissions even at `min`. Running stages drain and are never preempted.
- **Waiters.** Lowering preserves live holders and FIFO waiter order. Admissions resume only when every required group permits, under ADR-087's all-or-nothing acquisition. A longer wait can run past the MCP waiter's 3h cap, leaving the stage `awaiting_host_dispatch` for re-dispatch.
- **The 409.** The `concurrency` block gains `limit_source: spec|controller|override|shadow` and the tripped signal.
- **Audit.** The audit categories (all registered in `categories.go`, operator-visible ones in `issuecomment` `activityCategories`, surfaced by #4243) are:
  - `stage_concurrency_limit_changed`;
  - `stage_concurrency_limit_shadow`;
  - `stage_concurrency_limit_overridden`;
  - `stage_concurrency_limit_override_expired`.

**D5 Operator override (ADR-089 unchanged).**
- **The verb.** `fishhawk_set_concurrency_limit` sets an `auto` group's limit within `[min, max]`, with a required reason and an expiry (default 2h, maximum 24h, enforced on server time).
- **Callers.** The driving operator (`write:runs`, matching `fishhawk_promote_queued_stage`) or the captain, with account membership and authority over the target group.
- **The first officer** may only RECOMMEND a value, in an advisory `ops_monitor_alert` finding (ADR-089 D9, unchanged).
- **Refusals.** A run-bound `fhm_` token can never reach the verb, and the handler also refuses `actor_kind=agent` subjects `first_officer/*`.
- **Audit and limits.** The override and its actor are committed and audited atomically. An override never overrides critical-pressure blocking (D4).
- **Required counterfactual:** a seat token calling the verb gets 403 plus an audit row.

**D6 Staged enablement: shadow, then canary, then enforce. Each step is a captain act.**
- **Shadow (the default).** The integer `limit` governs. The controller records the limit it would have chosen, with reasons. Then two checks run:
  - **Lowering.** The shadow limit dropped at least one interval before the first category-C `host_overloaded:` classification, or `stage_orphan_sweep_degraded`, on that host, and holders at that moment exceeded the shadow limit.
  - **Raising.** Count intervals where a queued waiter existed while the host was healthy and the group was saturated for at least N samples.

  A campaign with no incidents is INCONCLUSIVE for the lowering check and does not qualify.
- **Pre-registration.** The incident definitions, minimum exposure (at least one campaign), warning lead time, false-alarm rate and queue-delay thresholds are fixed BEFORE the shadow run starts. Shadow mode alone cannot show a higher limit is safe, because a higher limit never actually runs (unlike ADR-098, where the full pass still runs). So shadow mode also replays recorded overload traces through the controller and injects probe failures.
- **Canary.** Shadow qualification enables a bounded canary: one host, `max` capped at the integer `limit + 1`, for one campaign, with pre-registered throughput, overload and rollback thresholds. The canary's measured result governs whether `enforce` is enabled.
- **Rollback.** Set the mode to `off` or `shadow`; the integer `limit` resumes at once.

**D7 Footprint measurement now; weighted admission is not part of this ADR.**
- **What is recorded.** The runner records each stage type's peak RSS and CPU per repository. It extends `procsweep`'s 2-second descendant sample with RSS, plus `docker stats` for the gate container. Descendants and containers are included regardless of language. The sampling residual applies.
- **Q4 is unchanged.** Weighted admission is revisited only on a measured campaign showing plan stages wasting capacity.

**Ratification questions**
- **Q1:** Should `auto` be a sibling `{min, max}` block beside the integer `limit`, with off and shadow keeping `limit` and fail-closed validation?
- **Q2:** Should `auto` be allowed on ONE host-kind group per host only, with quota and exclusive groups refusing it and undeclared groups treated as exclusive?
- **Q3:** Should the controller live in fishhawkd with persisted, fenced state, the staleness rule (stale or unknown goes to `min`), the saturated-only raise, the recovery-below-trip thresholds and the exponential back-off?
- **Q4:** Should the override be operator- or captain-only (`write:runs`), with the first officer recommend-only per ADR-089 D9 and run-bound tokens refused?
- **Q5:** Should enablement be staged shadow, then canary, then enforce, with pre-registered criteria, independent incident evidence, trace replay, and "no incidents means inconclusive"?
- **Q6:** Should footprints be recorded now, with ADR-094 Q4 unchanged?

## Decision

**Accepted (2026-10-14), ratified by the Captain as recommended.** Adopt Option C with D1–D7 as written in the Recommendation.

- **Q1: yes.** `auto: {min, max}` is a sibling block beside the integer `limit`. Off and shadow keep `limit`, and validation fails closed.
- **Q2: yes.** `auto` is allowed on ONE host-kind group per host. Quota and exclusive groups refuse it, and undeclared groups are exclusive.
- **Q3: yes.** The controller lives in fishhawkd with persisted, fenced state. Stale or unknown telemetry goes to `min`. Raising happens only when saturated, with recovery thresholds below trip thresholds and an exponential back-off.
- **Q4: yes.** The override is operator- or captain-only (`write:runs`). The first officer is recommend-only (ADR-089 D9), and run-bound tokens are refused.
- **Q5: yes.** Enablement is staged shadow, then canary, then enforce, each a captain act, with pre-registered criteria, independent incident evidence, trace replay and fault injection. "No incidents" is inconclusive.
- **Q6: yes.** Footprints are recorded now. ADR-094 Q4 is unchanged.

Implementation depends on ADR-094's slot store and capacity group (#3969, #4240–#4243). Until #4241 lands, `auto` applies to the ADR-087 implement group.

## Consequences

- **Throughput and safety.** In `enforce`, the host runs as many stages as headroom allows within `[min, max]`. Overload sheds new admissions within about one interval. Stale or unknown telemetry falls back to `min`, and running stages are never preempted.
- **Portability.** The probes are domain-scoped. Unsupported domains are `not_applicable` and keep the integer limit. Quota and exclusive groups keep their own semantics. Each repository calibrates in shadow mode before any canary.
- **Schema.** workflow-v2 `concurrency` gains:
  - `auto: {min, max}`;
  - a group `kind`;
  - the `1 ≤ min ≤ limit ≤ max ≤ 64` constraint.
  It is additive under the current major: run `scripts/sync-schemas`, then update `docs/spec/workflow-v2.md`, the generated site reference, and the `fishhawk init` template (`auto` absent by default).
- **Wire.** The host-dispatch marker gains optional headroom readings and `FISHHAWK_HOST_CAPACITY_AUTO`, and the 409 `concurrency` block gains `limit_source`. An old client sends neither, so it gets the integer limit (fail closed).
- **Docs.**
  - An operator page under `site/`: how `auto` works, the modes, the override and the canary.
  - Pointer rows in `docs/ARCHITECTURE.md`.
  - An auth checklist line for the new verb.
- **Residuals.**
  - Readings are client-supplied, and `max` is the trust bound.
  - Probes sample, so a burst within one interval is missed.
  - Host signals cannot see another tenant on a shared host.
  - A longer wait can outlive the 3h waiter cap.
- **Dependencies.** This needs ADR-094's slot store and capacity group (#3969, #4240–#4243, all unimplemented). Until #4241 lands, `auto` applies to the ADR-087 implement group. It shares probes with #4330.
- **Implementation (proposed slices):**
  1. The headroom interface plus macOS and Linux probes, shared with #4330, with readings on the marker and on waiter polls.
  2. Schema: the `auto` block, `kind`, validation, sync and docs.
  3. Persisted, fenced controller state and admission under the group locks, in mode `shadow`, with the audit categories and #4243.
  4. The override verb, with authz and its counterfactual.
  5. Runner footprint sampling (`procsweep` RSS plus `docker stats`).
  6. Trace replay and fault injection, plus the pre-registered shadow report.
  7. Canary, then enforce, each a captain act.

## Relations

- **ADR-087:** amends its "the ADMITTING stage's limit governs" rule (for `auto` groups in `enforce`, the stored controller value governs) and its "the queue serializes local implement throughput by design" consequence.
- **ADR-094:** amends D4 ("declared BY THE HOST … Default 3") and Q1. The host still declares the bounds, and a repository declaration can never widen them past the host's. D1–D3, D5–D8 and Q4 are unchanged.
- **Unchanged:** ADR-089 D2/D9 and ADR-097 D5.
- **Precedent:** ADR-098 (shadow-first, and a new ADR amending an accepted one).
- **Related:** #4330 (probe source), #4142 (ops monitor), #4148, #4243, ADR-090/092 (exclusive merge-candidate line), #3663 (starvation classifier).
