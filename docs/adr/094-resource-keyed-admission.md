---
id: ADR-094
title: "Resource-keyed admission: one admission path for every spawning verb, stages hold a scoped SET of resource groups including a host capacity group, and review rounds admit through provider-account groups that park on quota (amends ADR-087)"
status: accepted
date: 2026-10-10
issue: https://github.com/kuhlman-labs/fishhawk/issues/4239
supersedes: ["ADR-087"]
superseded_by: []
amended_by: ["ADR-100", "ADR-115"]
applies_to: []
---

# ADR-094: Resource-keyed admission: one admission path for every spawning verb, stages hold a scoped SET of resource groups including a host capacity group, and review rounds admit through provider-account groups that park on quota (amends ADR-087)

## Context

ADR-087 gave Fishhawk server-coordinated admission for host-dispatched stages. Its slot store, FIFO waiters, atomic single-transaction admission and the `concurrency_slot_queued` 409 work. Its model, however, is "one group per stage, keyed by stage type", and that no longer matches what is actually scarce.

**What admission covers today (2026-10-10):**
- **Implement:** the host default group `local-implement:<host>`, limit 2 in this repository's spec (`.fishhawk/workflows.yaml`, both implement stages).
- **Acceptance:** a named group `local-acceptance` at limit 1. It is the #4063 option-2 safety net, because every acceptance stage on a host shares one preview target: `localhost:8090`, the `fishhawk_preview` database, one pid file and one worktree.
- **Merge-candidate verify passes:** a `local-verify` group (ADR-092, implementation #4200, in flight).
- **Merges:** the ADR-093 merge queue. Its job is ordering plus merge-on-pass, not capacity.
- **Plan stages:** no group; they are host-dispatched agent processes.
- **Review rounds:** not admitted at all. Plan and implement reviews run as fishhawkd goroutines (`s.bgReviews`) that call model providers, with no concurrency limit. A provider quota exhaustion fails each invocation separately: codex is `usage limit` until 2026-10-13, so every round records a `failed` reviewer, and since #3913 `reviewer_unavailable` blocks a gating review.

**Where the model is wrong:**
1. **Limits are counted per stage type, not per resource.** Host CPU, memory, the Docker daemon (container-path gates each start their own Postgres service, #2137) and disk are shared by implement, acceptance, verify-only and plan stages. Each group counts only its own type, so the host carries the sum. On 2026-10-10 at 13:43 the host had 3 live runners (2 implement + 1 acceptance) under an implement limit of 2. That was fine on a 14-core host with 16 GiB for Docker, but nothing bounds it, and a verify pass under #4200 adds a fourth.
2. **Group scope is fixed.** A named group is REPOSITORY-scoped by schema. The real resources come in three scopes:
   - host: CPU, Docker, the preview port;
   - repository and base: one verify pass per base, the merge queue;
   - provider account: API quota.
   `local-acceptance` names a HOST resource with a repository-scoped key, so two repositories on one host would both bind `localhost:8090`. A host-scoped key would equally mis-serialize two hosts working on one repository's deploy target.
3. **One group per stage.** An acceptance stage needs BOTH a host slot and the preview target. A verify pass needs both a host slot and its per-base serial slot. The schema can express only one.
4. **Bypasses.** `run_stage`, `drive_run` and `run_children` do not use the slot queue (#3969, ADR-087's own part 2, still open). A campaign's limit holds only because its driver happens to use `dispatch_stage`.

## Options

- **A. A queue per stage type.** One queue each for plan, implement, acceptance, verify and review.
  - Mirrors the question as asked, but stage type is not the resource.
  - It does not bound total host load, and it multiplies queue machinery.
  - Rejected.
- **B. Status quo plus #3969.** Close the bypasses and keep one-group-per-stage.
  - Cheapest.
  - Host oversubscription, the scope mismatch and unbounded review rounds remain.
- **C. Resource-keyed admission on ADR-087's slot store (RECOMMENDED).**
  - Every runner-spawning or agent-starting verb admits through ONE path.
  - A stage holds a SET of resource groups, acquired all-or-nothing.
  - Each group carries an explicit scope (`host`, `repository`, `account`).
  - Every host-dispatched stage also holds the host capacity group.
  - Review rounds admit through provider-account groups that park on quota exhaustion instead of failing.
- **D. An external scheduler** (Kubernetes Jobs, Nomad).
  - Real resource accounting, but it does not exist on a laptop running the local loop, and the local loop is the product's primary path today.
  - Revisit for the hosted runner path, not here.

## Recommendation

**Option C.** It keeps ADR-087's proven core (atomic admission, a queue row plus a response block rather than a new stage state, a fail-closed 409, a bounded waiter) and changes only what a slot is keyed on. Proposed decision points:

**D1 One admission path.** Every verb that spawns a runner or starts agent work admits through the slot store. That covers `dispatch_stage`, `run_stage`, `drive_run`, `run_children`, campaign auto-dispatch, ADR-092 verify-only passes and ADR-093 head-of-line passes. A verb that cannot admit refuses with the 409 and never spawns. #3969 is the implementation of D1 and is unchanged in intent.

**D2 Multi-group holding.** A stage holds a set of groups.
- Admission acquires every group in the set inside ONE transaction, under the existing transaction-scoped try-lock, in a canonical (sorted-key) order. A miss on ANY group answers queued and holds NOTHING, so there is no partial holding and no lock-order deadlock.
- Queue order is strict FIFO per group. A waiter is admitted when every group in its set has a free slot AND no earlier waiter in any of those groups is still queued. This accepts head-of-line blocking over starvation.
- Release frees the whole set at once, through the existing reap paths.

**D3 Explicit scope.** A group key is `<scope>:<scope-id>:<name>`, where scope is one of:
- `host` (scope-id = the ADR-087 host label);
- `repository` (owner/name);
- `account` (provider + account).

The ADR-087 default `local-implement:<host>` maps to `host:<label>:implement`. Existing named groups keep their repository scope by default, so no existing spec changes meaning.

**D4 Host capacity group.** Every host-dispatched stage also holds one slot of `host:<label>:capacity`, whatever its type. This covers plan, implement, acceptance and verify-only.
- The limit is declared BY THE HOST, not the repository spec: the MCP process sends it on the host-dispatch marker beside the host label (`FISHHAWK_HOST_CAPACITY`). The most recent declaration governs. Default 3.
- Type groups stay as narrower caps inside it. For example implement limit 2 inside capacity 3 leaves room for one acceptance or verify-only stage.
- Integer slots only, no weights (Q4).

**D5 Default resource sets.**
- Implement: `{host capacity, host implement}`.
- Acceptance on the local preview target: `{host capacity, host:<label>:preview-target}`. This replaces this repository's repository-scoped `local-acceptance` with the correctly scoped host group, until per-run previews make the target per-run.
- Verify-only (ADR-092): `{host capacity, repository:<repo>:verify:<base>}`.
- Plan: `{host capacity}`.
- A spec may ADD groups to a stage's default set. It may not remove the host capacity group.

**D6 Review-round admission.** A plan or implement review invocation admits through `account:<provider>/<account>:review` before calling the provider.
- The limit is a fishhawkd configuration value per provider, default 4.
- **Quota exhaustion:** when a provider answers with a quota-exhaustion error carrying a reset time (codex `try again at …`, an Anthropic 429 with `retry-after`), the group is marked PARKED until that time. Later invocations queue instead of failing.
- **Stated wait:** a round whose reviewer is parked past the round's own budget records that reviewer as `reviewer_unavailable` with the park's reset time, never a bare `failed`.
- **Persistence:** park and waiter state persist, so the ADR-091 boot re-dispatch resumes queued rounds rather than losing them.
- **What this does NOT change:** #3913's gating rule (an unavailable reviewer still blocks a gating review). Rounds stop burning attempts against a known-exhausted quota, and the operator sees why.

**D7 Visibility.** One read surface lists every group's holders, waiters, scope, limit and park state.
- The existing per-stage `concurrency` block grows a `groups[]` list.
- `fishhawk_doctor` reports this host's capacity declaration and current holders.
- Every admit, queue and park writes an audit row: the existing `stage_concurrency_queued`/`_admitted`, plus a registered `review_admission_parked`.

**D8 The merge queue stays separate.** ADR-093 ordering and merge-on-pass are not capacity. Its verify passes admit through D1 like any other stage.

**Ratification questions:**
- **Q1:** Is the host capacity limit host-declared (MCP env, sent on the marker; default 3), with the residual that it is client-supplied like the host label?
- **Q2:** Strict per-group FIFO with head-of-line blocking (D2), rather than skip-ahead admission that risks starving multi-group stages?
- **Q3:** Review provider limit default 4, and park-on-quota (D6)?
- **Q4:** Integer slots, no per-stage weights, with weights revisited only if a measured campaign shows plan stages wasting capacity?
- **Q5:** Does this partially supersede ADR-087, replacing its "one group per stage" and "named group is repository-scoped" rules while keeping every admission constraint?

## Decision

**Accepted (2026-10-10), ratified by the maintainer as recommended.** Adopt Option C, resource-keyed admission on ADR-087's slot store, with D1–D8 as written in the Recommendation.

Ratification answers:
- **Q1:** yes. The host capacity limit is host-declared: `FISHHAWK_HOST_CAPACITY` in the MCP process env, sent on the host-dispatch marker, default 3. Accepted residual: it is client-supplied, like the host label, and not a security boundary.
- **Q2:** strict per-group FIFO, accepting head-of-line blocking over starvation of multi-group stages.
- **Q3:** the review provider-account limit defaults to 4, with park-on-quota-exhaustion. A reviewer parked past its round budget records `reviewer_unavailable` with `parked_until`. #3913's gating rule is unchanged.
- **Q4:** integer slots, no per-stage weights. Revisit only on a measured campaign showing plan stages wasting capacity.
- **Q5:** yes. This partially supersedes ADR-087: its "one group per stage" and "a named group is repository-scoped and replaces the host default" rules. ADR-087's admission constraints all stand: atomic single-transaction admission, holding derived from stage state, prompt release on spawn failure, the fail-closed 409, queue row not stage state, and the bounded waiter.

Implementation:
1. #3969 (D1)
2. #4240 (D2/D3)
3. #4241 (D4/D5)
4. #4242 (D6)
5. #4243 (D7)

#4200 lands first under the ADR-087 model.

## Consequences

- **Bounded host load.** Total host load is bounded by one host-declared number, not by the sum of per-type limits. A fourth concurrent runner (the #4200 verify pass) can no longer oversubscribe the host silently.
- **Correct scope.** Groups are keyed on the resource they protect, so two repositories on one host no longer collide on the preview target, and a per-base verify serial is not host-coupled.
- **Reviews.** Review rounds stop burning attempts against an exhausted provider quota, and a parked reviewer reports its reset time.
- **Schema.** workflow-v2 `concurrency` gains an additive `groups: [{group, scope, limit}]` list. The current `group`/`limit` pair remains as a single-group shorthand, mutually exclusive with `groups`. Run `scripts/sync-schemas` and update `docs/spec/workflow-v2.md` and the generated site reference.
- **Wire.** The host-dispatch marker gains an optional `capacity` beside `host`. Version skew is fail-closed as in ADR-087: an old client sends no capacity, so the host gets the default 3.
- **Residuals.**
  - The host capacity and host label are client-supplied, not a security boundary.
  - Integer slots overcount light plan stages.
  - Strict FIFO accepts head-of-line blocking.
  - Review admission is per fishhawkd process until a multi-replica deployment needs a DB-held park state.
- **Implementation slices:**
  1. **#3969 (D1).**
  2. Scoped group keys plus multi-group holding in the slot store and schema (D2/D3).
  3. The host capacity group, host-declared capacity and default resource sets, plus this repository's spec moving `local-acceptance` to `host:preview-target` (D4/D5; the spec edit is operator-authored).
  4. Review-round admission and quota parking (D6).
  5. The unified read surface and doctor (D7).
  #4200 lands under the current ADR-087 model and gains the host capacity group in slice 3.

## Relations

Amends ADR-087 (#3968). Related: ADR-092 (#4200), ADR-093, ADR-091 (boot re-dispatch), #3913 (`reviewer_unavailable` blocks gating), #4063 (shared preview target), #3969.

## Amendment A1: legacy group-key strings stay canonical

Ratification: Captain, 2026-11-19 (#4240); final text confirmed by the Captain at merge.

**Context.** D3 writes the default group as `host:<label>:implement`. The legacy key strings are load-bearing: they are persisted in `stage_concurrency_slots` rows and `specialist_jobs.admission_group`, hashed into the advisory-lock keys, mirrored by the MCP host-label parity test, and asserted by the ADR-087 tests. A string rewrite would change every one of them across a deploy. Implementing D2/D3 (#4240) also has to fix the slot-store row shape, the FIFO formula for group sets, how a promotion applies to a set, what a declared `groups` list means before D5 lands, and who may set a host-scoped group's limit. The Captain ruled on these in the 2026-11-19 ruling comment on #4240 (constraints R1–R7).

**Decision.**
1. **Legacy keys are canonical.** D3's "maps to" is an IDENTITY mapping, not a string rewrite. `local-implement:<host>`, `local-verify:<host>` and `spec:<repo>:<name>` are the canonical keys of host/`implement`, host/`verify` and repository/`<name>`. Only new identities render as `<scope>:<scope-id>:<name>` (e.g. `host:<label>:preview-target`, `account:<provider>/<account>:review`). The aliases `repository:<repo>:<name>`, `host:<id>:implement` and `host:<id>:verify` are rejected, so every key has exactly one spelling and round-trips, and a scope-id may not contain `:`. D5's verify-only identity is spelled `spec:<repo>:verify:<base>`.
2. **One slot row per (stage, group).** The `stage_concurrency_slots` primary key widens to `(stage_id, group_key)`. A stage's episode state (state, enqueued_at, last_seen_at, order_key, acquired_at, held_dispatched_at, admission_nonce) is shared across its rows and written together, from one timestamp per statement.
3. **D2's strict per-group FIFO beside ADR-087's reservation formula.** A set is admitted only when, for EVERY group G in it: (a) holders + job holders + earlier live waiters < limit, AND (b) no earlier live waiter queued on G belongs to a set of more than one group. Condition (b) is vacuous when every earlier waiter is single-group, so ADR-087 admission is unchanged. Stated as the operator reads it: a later waiter on G is refused when (i) an earlier live waiter on G has a multi-group set, or (ii) no slot of G remains after reserving one per earlier live waiter. The mixed case is explicit: a SINGLE-group earlier waiter at limit >= 2, with a free unreserved slot, does not block a later waiter, whether that later waiter's own set is single- or multi-group; this is ADR-087's behaviour, unchanged.
4. **Promotion applies one key to the whole set (#4191 option a).** A promoted stage gets ONE order key, written to every row of its set in one statement, under blocking group locks taken in sorted-key order (Admit only try-locks, so no wait cycle forms). Admission stays all-or-nothing, so a promoted stage never holds one group while queued in another.
5. **A declared `groups` list REPLACES the default set.** Until #4241 applies D5's add-only rule, a workflow-v2 `concurrency.groups` list is the stage's whole set; an author keeps the host implement group by listing `{group: implement, scope: host}`. This is PROVISIONAL: #4241 changes it to D5's "a spec may ADD groups" rule.
6. **A repository spec cannot set a host-scoped group's limit.** A host-scoped group is shared by EVERY repository dispatching on that host, so it is a cross-repository trust boundary: one repository's spec must not widen or narrow a resource every other repository on the host competes for. A `concurrency.groups` entry with scope `host` therefore carries no `limit`, for any name; both validators refuse it. The host's legacy implement group (`local-implement:<host>`) is the one declarable legacy host group, and it keeps the host default; any other declarable host-scoped group is resolved at limit 1. The host verify group (`local-verify:<host>`) is reserved for server-decided merge-candidate verify passes and specialist jobs and is not spec-declarable. Host-level limits are the host operator's to declare under D4 (#4241). Repository-scoped entries keep their 1..64 limit. The single-group shorthand `concurrency: {limit: N}` is unchanged, because single-group behaviour must stay byte-identical.

**Consequences.**
- No key string, advisory-lock key or persisted row is rewritten across the deploy.
- The binary and the schema change together. Stop every old fishhawkd, including every API replica, migrate up, then start the new binary; a rolling upgrade is unsupported for this release, because each binary's `ON CONFLICT` target needs its own primary key. Rollback is the mirror image: stop the new binary, migrate down, start the reverted binary.
- The down migration fails closed. It refuses, naming the stage, while any stage holds more than one `held` row, because collapsing a live multi-group hold would make the dropped groups look free; drain those stages first. A queued-only multi-row stage collapses to its canonical-first row and restarts at the tail on its next poll.
- Before rolling back, confirm that no non-terminal run's captured workflow spec declares `concurrency.groups`, or cancel those runs: a reverted binary cannot parse such a spec and falls back to the default group.
- Store-side, a group set may hold up to 16 groups, above the schema's 8 entries, so #4241 can add the host capacity group to a full declared set without a store change.
- The read surfaces gain an additive `groups[]` list (D7's first step); their existing top-level fields describe the binding group.

## Amendment A2: host capacity per request, add-only default sets, and the capacity FIFO exemption

Ratification: Captain ruling 2026-11-20 (#4241) for the capacity group's exemption from FIFO rule (b) and for the landing order before #3969; the per-request capacity (C3) and the other items through the first officer's 2026-11-20 constraint on #4241, Captain-ratified; final text, and the reservation reading in item 3, confirmed by the Captain at merge.

**Context.** #4241 implements D4 (host capacity group) and D5 (default resource sets) on the multi-group slot store from #4240. Doing so has to settle what a declared set means now that defaults exist, where the capacity limit lives, how FIFO rule (b) of A1 decision 3 treats a group that every stage holds, how verify-only passes are keyed, and what the existing shorthands now mean. The items below are the constraints C1 to C6 of the #4241 implementation plan.

**Decision.**
1. **Every declaration ADDS to the default set.** This replaces A1 decision 5, which was provisional. A workflow-v2 `concurrency.groups` list adds each entry to the stage type's default set; no declaration form removes a default group, and none removes the host capacity group (D5).
2. **The capacity key and a PER-REQUEST limit (C3).** The capacity group's key is `host:<label>:capacity`; an empty label maps to `unknown`. Every stage that reaches the host-dispatch marker's admission arm holds one slot of it: plan, implement, acceptance and verify-only stages, and also agent review stages and any other agent-executed, host-dispatched stage. The limit is a property of the marker REQUEST: the marker's optional `capacity` (an integer 1..64, which the MCP process sends from `FISHHAWK_HOST_CAPACITY`) sizes THAT admission only, and a request without it uses 3. Nothing is stored, so there is no per-host declaration registry and a client cannot resize another client's capacity.
   - **This supersedes the issue's wording.** #4241's "the server keeps the most recent declaration per host" and D4's "The most recent declaration governs" are replaced by the per-request rule above, as the first officer's constraint C3 directs.
   - **Consequence.** Processes that dispatch on one host label with different env values each admit against their own limit. For example, a stdio MCP process with `FISHHAWK_HOST_CAPACITY=4` and fishhawkd's `/mcp` route with it unset admit against 4 and 3 respectively. This follows ADR-087's rule that the admitting stage's limit governs. The operator sets the same value in every process that dispatches for a host; `fishhawk_doctor` shows each process's value and its source.
   - **Advisory, not a security boundary (Q1).** The capacity is client-supplied, like the host label. A hostile caller holding `write:runs` can send any capacity in 1..64 or any label, so capacity bounds a cooperating host's load and does not contain a hostile caller.
   - **Beside D6 (#4242).** D6's `account:<provider>/<account>:review` group admits fishhawkd review rounds against provider quota. Those rounds run as fishhawkd goroutines, not on the dispatching host, so they hold no host capacity slot; an agent review stage that IS host-dispatched holds one. The two groups count different resources, and D6 is unchanged.
3. **The capacity group is exempt from FIFO rule (b) (C1).** Captain ruling 2026-11-20 (#4241). On `host:<label>:capacity` admission applies only condition (a) of A1 decision 3, ADR-087's reservation formula: holders + job holders + earlier live waiters < limit. Rule (b) still applies to every other group.
   - **Reservation consequence.** Each earlier live waiter reserves one capacity slot, even a waiter that is blocked only on `local-implement:<host>`. A queued waiter is not a FIFO barrier on capacity: it counts as one reserved slot, so capacity blocks a later stage only when holders plus earlier reservations fill capacity. Example: at capacity 3 with implement limit 2, two holding implements plus one queued implement fill capacity (2 + 1), so a later plan, acceptance or verify pass queues. At capacity 4, one slot stays free for them.
   - **A fail-closed dispatch still reserves, until #3969.** When `fishhawk_run_stage`, `fishhawk_drive_run` or `fishhawk_run_children` fails closed on a queued dispatch (item 4), it leaves a waiterless queued set: no waiter re-polls its rows. That set still reserves one capacity slot until it goes stale past `QueueTTL` (60s), so for up to 60s it can queue a later stage that would otherwise be admitted.
   - A verify pass queued after an implement is therefore admitted on capacity unless holders plus earlier reservations fill capacity; on `local-verify:<host>` rule (b) still applies among verify passes.
4. **Landing order before #3969 (C2).** This slice lands before #3969 (D1), and #3969 is the next item. Until #3969 lands, `fishhawk_run_stage`, `fishhawk_drive_run` and `fishhawk_run_children` fail closed (no spawn) when a plan or acceptance stage's dispatch queues. On capacity, that happens only when holders plus earlier reservations fill capacity. Implement stages keep their existing behaviour. The host dispatcher (ADR-104) and `fishhawk_dispatch_stage` queue correctly.
5. **Verify-only passes keep the host verify group (C5).** A merge-candidate verify-only pass resolves to `{host:<label>:capacity, local-verify:<host>}`, NOT D5's per-base `spec:<repo>:verify:<base>`. This keeps ADR-092 D3's host verify group, per the first officer's #4241 carry-forward item 5.
6. **The named shorthand on an implement stage (C6).** On an implement stage, `{group: X, limit: N}` now holds `spec:<repo>:X` at N AND `local-implement:<host>` at its default limit 1. A spec cannot raise that limit: a host-scoped `groups` entry carries no limit (A1 decision 6), and `groups` is mutually exclusive with the shorthand. No in-spec remedy exists. The only way to run N implements in parallel is the `{limit: N}` shorthand, which gives up the named group; a host-declared implement limit is future work. `{limit: N}` declares `local-implement:<host>` at N: it resizes the implement default on an implement stage and adds the group on any other stage type.
7. **The rest of the surface.**
   - `{group: capacity, scope: host}` is reserved. Both validators and the server's declared-group resolution refuse it after comparing the CANONICAL name, with scope and name whitespace-trimmed and lowercased. The schema's group pattern already refuses a case or whitespace variant in a document, so the fold is the backstop for a spec built in code.
   - `preview-target` is a built-in host group, `host:<label>:preview-target`, held at limit 1. There is no host knob for its limit yet.
   - The marker's `capacity` field: absent or JSON null means no declaration (limit 3); any other value outside 1..64, or a non-integer, is refused with 400 `validation_failed` naming `capacity`, before any stage read or slot row. `FISHHAWK_HOST_CAPACITY` unset sends nothing; a set value is trimmed and parsed as a base-10 integer in 1..64. Anything else, including a set but empty value, fails closed: `fishhawk-mcp` and the host dispatcher refuse to start, and fishhawkd's `/mcp` route, which reads the variable lazily, fails closed at marker time (no spawn).
   - **Binding preference.** A set's top-level read fields (group, limit, position, holders) describe the stage-type group, not the capacity group. While queued, the binding group is the one at the highest position, and a tie prefers a non-capacity group, then canonical order. Once admitted or held, it is the first non-capacity group in canonical order. The capacity group binds only when it alone is strictly highest, or is the only group. Sets without the capacity key behave as before.

**Consequences.**
- No migration. Capacity rows are ordinary `stage_concurrency_slots` rows, and the slot-row shape is unchanged.
- Version skew is fail-closed, as in ADR-087. A client with `FISHHAWK_HOST_CAPACITY` unset is byte-identical to an old client and gets 3. A client with it set, talking to an older fishhawkd, gets 400 and no spawn, because the marker body refuses unknown fields. Upgrade fishhawkd before any client sets the variable.
- Upgrade: on its first poll after the upgrade, every live queued waiter asks for a larger set. It gains `host:<host>:capacity`, and an implement whose declaration did not name the host implement group (the named shorthand of item 6, or a `groups` list without `{group: implement, scope: host}`) also gains `local-implement:<host>`. Its row set then differs from the requested set, so the store restarts it at the queue tail and clears its `order_key`. Waiters therefore re-queue in the order they next poll, and any operator promotion (`fishhawk_promote_queued_stage`) is lost: re-promote after the upgrade. Stages admitted before the upgrade hold no capacity row (and such an implement holds no `local-implement:<host>` row), so capacity under-counts until those stages settle.
- Rollback: unset `FISHHAWK_HOST_CAPACITY` in every MCP and host-dispatcher env, then revert the binary. A reverted binary never names the capacity group, so held capacity rows count for nothing, and a queued set that differs restarts at the tail on its next poll. No declaration state exists to clear.
- Captured specs in non-terminal runs take the add-only meaning at deploy, and revert to A1's replace meaning on rollback.
- A host running implement limit 2 should set `FISHHAWK_HOST_CAPACITY=4`, so that two holding implements and one queued implement still leave a slot for a plan, acceptance or verify pass (item 3).

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

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2182} -->

## Amendment A3: every host-spawn verb queues (#3969)

Ratification: pending. The Captain confirms this text at merge (per the 2026-11-20 comment on #3969). It changes no front-matter field.

**Context.** D1 requires every verb that spawns a runner to admit through the slot store, and says "A verb that cannot admit refuses with the 409 and never spawns." Under ADR-087 (#3964) only `fishhawk_dispatch_stage`, and later the host dispatcher (ADR-104), acted on that 409 by queueing: it left the stage `awaiting_host_dispatch` and started a slot waiter that spawns on admission. A2 item 4 recorded the landing-order interim, in which `fishhawk_run_stage`, `fishhawk_drive_run` and `fishhawk_run_children` failed closed on a queued dispatch. A2 item 3's bullet "A fail-closed dispatch still reserves, until #3969" recorded its consequence: a waiterless queued set that reserves a capacity slot until it goes stale past `QueueTTL`. #3969 implements D1 for those three verbs.

**Decision.**
1. **D1's refusal now reads as a queue for every host-spawn verb.** For `fishhawk_run_stage`, `fishhawk_drive_run` and `fishhawk_run_children`, D1's "A verb that cannot admit refuses with the 409 and never spawns" now means: the verb QUEUES with a live waiter, which spawns exactly once on admission, as `fishhawk_dispatch_stage` already does. The 409 `concurrency_slot_queued` is still the server's answer, and no verb spawns at queue time; what changes is that no verb fails closed on it. This holds for every stage type (plan, implement, acceptance, review and verify passes), because every host-dispatched agent stage holds a host capacity slot (A2 item 2).
2. **Each verb's queued return.**
   - `fishhawk_run_stage` returns early instead of blocking to terminal: outcome `awaiting_concurrency_slot` (non-terminal), the `concurrency_slot` block and a `fishhawk_await_stage` next step. Its waiter spawns the runner detached.
   - `fishhawk_drive_run` stops with the stop reason `awaiting_concurrency_slot`, not `host_dispatch_failed`. A waiter the drive starts spawns through the drive's own spawn path. Its next actions are `fishhawk_await_stage`, then `fishhawk_drive_run` to resume.
   - `fishhawk_run_children` reports the child `queued`. A child it queues spends one unit of the per-invocation budget, like a spawned child, and a child queued with a live waiter counts as in flight. `fishhawk_await_children` does not announce a child queued with a live waiter as dispatchable.
3. **Spawn inputs are built at admission.** The waiter's prepare step receives the marker response that admitted the stage. That is the `transitioned:true` answer, or on the own-lost-admission arm the `transitioned:false` answer that followed the lost request. Each verb re-resolves the runner binary, re-composes the argv and rebuilds the env with the current token at admission (ADR-087). A decomposed child spawns against the base branch in the admitting response, so the server stays the authority on the per-wave re-base (#2363).
4. **A2 items 3 and 4: the interim is superseded.** A2 item 4's landing-order interim ends, because no host-spawn verb fails closed on a queued dispatch. A2 item 3's bullet "A fail-closed dispatch still reserves, until #3969" no longer describes a live path. Every host-spawn verb leaves a live waiter behind a queued set, so a capacity reservation ends at `QueueTTL` only after its waiter dies (its MCP process exits) or the waiter's cap expires. A2's reservation reading itself is unchanged: each earlier live waiter reserves one capacity slot. A2's ratified text is not rewritten; this amendment supersedes those two items' interim.

**Consequences.**
- Contract change: `fishhawk_run_stage`, which normally blocks to terminal, returns a non-terminal result on a queued dispatch. A client that reads `stage_state` as terminal must follow `next_step`. The new output fields are additive and omitted on every other return.
- No migration, REST or wire-schema change, and no stored state. Rollback is a revert of the MCP binary. The three verbs then fail closed again, and a live waiter dies with the reverted process, so its queue row goes stale after `QueueTTL` (60s).
- A waiter dies with its MCP process. The queued stage then reads `waiter_live: false`, and `fishhawk_await_stage` releases with a message naming the verb to re-invoke: `fishhawk_run_children` for a fan-out child, otherwise the verb that queued it.

**Risks and residuals.**
- **Acceptance pre-marker gates run at queue time and are not re-run.** An acceptance stage's pre-marker gates (the admission short-circuit and the target probe) run before the marker, at queue time. They are NOT re-run when the waiter admits the stage, on the admitted arm or on the own-lost-admission arm, for every verb; this matches `fishhawk_dispatch_stage` under ADR-087. A target that goes away while the stage is queued is caught only by the runner's own target check (category C `acceptance_target_unreachable`).
- Death between admission and spawn leaves the stage `dispatched` with no runner until the 15-minute `DispatchedStaleAfter` backstop, the dispatch watchdog or `fishhawk_reap_stage` releases it, as under ADR-087.
- `fishhawk_drive_run` records its `run_auto_driven` act before the marker, so a queued stop leaves an act row with no spawn yet. Re-invoking the drive while the stage is still queued records one more row, noted `retry`.
- The waiter reports a category C failure even when a spawn is refused because a cancel landed between admission and spawn (`errRunCancelledBeforeSpawn`). `fishhawk_run_children`'s immediate path does not report in that case. This is pre-existing and unchanged.
- `effective_cap` stays a per-invocation budget and does not bound live runners across concurrent invocations.
- **One waiter registry serves all four verbs.** The MCP process keeps one waiter per stage, shared by `fishhawk_dispatch_stage`, `fishhawk_run_stage`, `fishhawk_drive_run` and `fishhawk_run_children`. On `already_waiting`, the FIRST queuing call's spawn inputs govern: its `push_and_open_pr`, `fishhawk_run_children`'s `--parallel-isolate` and admitting base branch, and its spawn path. The current call's inputs are dropped, and the queued warning says so. This is pre-existing for a repeat `fishhawk_dispatch_stage`; what is new is that verbs with different spawn semantics now share the registry.
- **A `transitioned:false` inside a live waiter's window does not spawn.** `fishhawk_run_stage` and `fishhawk_drive_run` proceed on a 200 `transitioned:false` (the dead-runner re-dispatch) only when no waiter in this process held the stage between the verb's stage read and the marker's answer. When one did, the verb spawns nothing and returns the queued state: that waiter may own the spawn, but it may also have exited without admission (cap expiry, a terminal refusal, or another session dispatched the stage). If no runner appears, re-invoke the verb after the waiter exits. `fishhawk_dispatch_stage` keeps its pre-existing proceed on that arm. A waiter in another MCP process is not visible to this check.

## Amendment A4: a capacity reservation counts only a capacity-bound waiter

Ratification: Captain, 2026-11-27 (issue #5608), ratifying the text exactly as converged in the operator's ADR review (docs/adr/README.md § "Drafting and review"; round 3: Fable claude-fable-5-2 and Astra gpt-6-astra both APPROVE, at repo commit d991330ad). Recorded by the operator. It changes no front-matter field.

**Context.** A2 item 3 exempts `host:<label>:capacity` from FIFO rule (b) but keeps condition (a), so "each earlier live waiter reserves one capacity slot, even a waiter that is blocked only on `local-implement:<host>`" (A2 item 3; `backend/internal/concurrency/store.go:401-402`; `backend/internal/concurrency/README.md:341-352`). A queued implement therefore holds a capacity slot it cannot use. At capacity 3 with implement limit 2, two holding implements and one queued implement fill capacity, so a plan, acceptance or verify pass queues. That makes D4's example false: "implement limit 2 inside capacity 3 leaves room for one acceptance or verify-only stage". A2's Consequences advise capacity 4 for an implement limit of 2. That is no fix either, because "each further queued implement reserves one more slot" (`README.md:351-352`). The defect is the reservation reading, not the default.

**Decision.**
1. **The reservation counts only a capacity-bound waiter.** On `host:<label>:capacity`, condition (a) of A1 item 3 becomes: holders + job holders + earlier live *capacity-bound* waiters < limit. An earlier live waiter W is capacity-bound when every group of W's set other than capacity would admit W now. "Would admit" means conditions (a) and (b) of A1 item 3 for W on that group, evaluated as `Admit` would evaluate W's own request at that instant. The evaluation uses the limit W's row for that group records, W's effective key, and that group's holders, job holders and fresh earlier waiters. A waiter blocked on any other group (an implement behind `local-implement:<host>`, a verify pass behind `local-verify:<host>`, an acceptance behind `host:<label>:preview-target`) reserves no capacity. A waiter whose set is `{capacity}` alone is always capacity-bound, so ADR-087's reservation is unchanged for it. The test needs no recursion through capacity, because it never asks whether capacity itself would admit W.
2. **Freshness is unchanged.** Only a row fresh within `QueueTTL` counts (`store.go:130-138`). A2 item 3's exemption from rule (b) on capacity stands, and rule (b) still orders every other group.
3. **A waiter keeps its FIFO position on capacity.** While W's rows stay fresh, they keep their `enqueued_at` (`store.go:461-467`). So the moment W's other groups would admit it, W counts as capacity-bound at its original position for every later admitter, even before W polls again. Fixture: capacity 3, implement limit 2. I1 and I2 hold. I3 queues on `{capacity, implement}`. Plan P1 is admitted (three capacity holders), and plan P2 queues on capacity. When I2 settles:
   - P2 is refused, because I3 is now capacity-bound and earlier: 2 holders + 1 reservation = 3.
   - I3 is admitted on its next poll.
   A waiter cannot lose capacity-bound status to a later stage. A later admitter on W's other group counts W as an earlier waiter there, under conditions (a) and (b). Only an operator promotion (A1 item 4) reorders it.
4. **Atomicity is unchanged.** Admission stays one transaction under the caller's try-locks, taken in sorted-key order (D2; `store.go:320`). The classification reads other groups' rows in that transaction without taking their locks. A concurrent change there can misclassify one earlier waiter for one decision. That affects only who is admitted first. Each admission checks live holder counts under the group's lock against the limit resolved for that request. Classification does not weaken that check. A lower limit on a later request does not preempt existing holders, and each request admits against the limit resolved for it, even when a later request resolves a different one. `TestAdmitSet_RacingCapacityAcrossTypes` pins the occupancy bound for stable, common limits.
5. **No hold-and-wait.** A reservation is not a hold. A set is still acquired all-or-nothing and released whole (D2, A1 item 4). A waiter that is not capacity-bound holds nothing and reserves no capacity slot. Its reservations and FIFO barriers on every non-capacity group remain unchanged.
6. **D4's example now holds.** At capacity 3 with implement limit 2, two holding implements and any number of queued implements leave one capacity slot for a plan, acceptance or verify pass. This item withdraws A2's Consequences line "A host running implement limit 2 should set `FISHHAWK_HOST_CAPACITY=4`" and A2 item 3's "Reservation consequence" example. Capacity is sized to the host (ADR-115 D3's doctor suggestion), not to the implement limit plus a margin. It also replaces A3 item 4's sentence "A2's reservation reading itself is unchanged: each earlier live waiter reserves one capacity slot": from this amendment, only a capacity-bound waiter reserves. A3 item 4's lifetime rule (a reservation ends at `QueueTTL` after its waiter dies) stands for a capacity-bound waiter.
7. **Account scoping is unchanged.** Every count, and the classification, stays within the run's account (`store.go:103`, `:135`).

**Consequences.**
- This is a store-only change, with no migration and no wire, schema or spec change. Rollback is a binary revert, which restores A2's reading on the next admission.
- The pinned fixtures for A2's reading change:
  - `TestAdmitSet_CapacityReservationCountsAsFull` inverts: the later plan is admitted.
  - `TestAdmitSet_StaleCapacityReservationEnds` is re-based on a capacity-bound waiter (a `{capacity}`-only stage), because I3 never reserved; it still pins that a stale row reserves nothing.
  - New tests pin item 1 (`{capacity}`-only waiters still reserve) and the item 3 fixture.
- Docs and surfaces updated in the implementing change:
  - `backend/internal/concurrency/README.md` § "Host capacity group";
  - the `fishhawk_dispatch_stage` tool description (`backend/internal/mcpserver/dispatch_stage.go:211-212`);
  - the `.env.example` capacity guidance.
- Read surfaces are unchanged. A queued stage's capacity `position` still counts every earlier live waiter, so it can overstate the wait.

**Risks and residuals.**
- **Classification cost.** Each capacity decision evaluates the other groups of every earlier fresh capacity waiter. That is bounded by queue depth times `MaxGroups` (16), as one more read set inside the admission transaction. It lengthens the capacity lock hold; a concurrent admitter that misses the lock queues and decides on its next poll, as today.
- **ADR-115's reserve is not decided here.** ADR-115 D3 reserves at least one capacity unit for merge-queue verify passes (#4655, unshipped). This amendment removes the reservations that queued implements took. A capacity-bound waiter of any type still precedes a later verify pass. On `local-verify:<host>`, rule (b) still orders verify passes. The reserved unit remains #4655's decision.
- **Second-in-line waiters.** A waiter behind an earlier multi-group waiter on a non-capacity group reserves no capacity. Later capacity-only stages may overtake it multiple times while that barrier remains. Once every other group would admit it, it reserves capacity at its original position.
- **Short waits after a release.** A waiter that becomes capacity-bound after a later stage took the slot waits for one more capacity release. The wait is bounded by holder settlement.

## Amendment A5: host limits have one declarer per host

Ratification: Captain, 2026-11-27 (issue #5609), ratifying the text exactly as converged in the operator's ADR review (docs/adr/README.md § "Drafting and review"; round 3: Fable claude-fable-5-2 and Astra gpt-6-astra both APPROVE, at repo commit d991330ad). Recorded by the operator. It changes no front-matter field.

**Context.** Under A2 item 2, the capacity limit is a property of each marker request. The MCP process sends it from `FISHHAWK_HOST_CAPACITY`, and "nothing is stored" (`backend/internal/server/stage_concurrency.go:77-83`, `:95-102`). The stdio `fishhawk-mcp`, `fishhawk-mcp host-dispatch` and fishhawkd's `/mcp` route each send their own value (`backend/internal/mcpserver/hostcapacity.go:79-88`), and A2 asks the operator to keep them identical.

On 2026-11-27 that drift was live on host `Mac` (#5592): the session stdio processes admitted at 3 while the dispatcher admitted at 4, so a plan queued at 3 reserved a slot against a verify pass at 4.

A2 item 6 leaves a host-declared implement limit as future work. ADR-104 Amendment A2 has since given the dispatcher an authenticated per-(account, host) heartbeat that can carry the declaration.

**Decision.**
1. **One declarer per (account, host label).** The host's limits are declared only by a principal holding a valid `host-dispatcher/<label>` token, through `POST /v0/host-dispatchers/heartbeat`. ADR-104 (#4524) Amendment A3 adds the fields.
   - The label comes from the token subject and the account from the token row, never from the body; every other subject is refused (`backend/internal/server/host_dispatcher_heartbeat.go:18-23`, `:51-57`, `:86-91`).
   - Only `fishhawkd token issue` mints a dispatcher token, which needs database access (`backend/cmd/fishhawkd/token.go:125-129`). `POST /v0/tokens` mints only the caller's own subject (`backend/internal/server/tokens.go:540-546`), and a dispatcher cannot reach it (`backend/internal/hostdispatcher/README.md:50`).
   - The declaration is the heartbeat's optional `host_capacity` and optional `host_implement_limit`, each 1..64 and each governing independently: an absent `host_capacity` falls through to item 2 (b), and an absent `host_implement_limit` leaves item 5's spec value. It is stored in that heartbeat's (account, label) row (migration 0121); the latest write wins.
   - A value outside 1..64 drops the whole declaration, never the heartbeat: liveness and status still store, and the response's `dropped_declaration` names the field. The stored row then carries no declaration, so no earlier declaration is carried forward. The row's payload records the dropped field name, so a read can tell 'dropped' from 'not declared'. A non-integer value is a structural error and is refused with 400, as ADR-104 A2 item 5 states.
2. **The governing capacity.** A marker admission for a run's account and the marker's host label sizes `host:<label>:capacity` from the first of these that applies:
   - (a) a valid `host_capacity` present in the fresh heartbeat row whose account exactly matches the run's account and whose label matches the marker's label; two untenanted accounts match. Freshness uses the server-stamped `received_at`, `StaleAfter(interval_seconds)` and `FutureSkew`. If `host_capacity` is absent, continue to (b), even when `host_implement_limit` is present.
   - (b) fishhawkd's `--default-host-capacity` (env `FISHHAWKD_DEFAULT_HOST_CAPACITY`), 1..64, applied to every label. A set value outside 1..64 refuses fishhawkd start-up. On the local stack, `scripts/dev` sets it from the same `.env` `FISHHAWK_HOST_CAPACITY`, so the declaration and the fallback agree.
   - (c) `DefaultHostCapacity` 3 (`backend/internal/concurrency/concurrency.go:55`).

   The server reads this once per marker request, before `Admit`, and passes it as that request's capacity limit (`resolveStageConcurrency`, `stage_concurrency.go:250`). The store, its single transaction and ADR-087's "the admitting stage's limit governs" are unchanged. A row admitted under an earlier value keeps holding. The match is exact, as the store's account partition is (`store.go:103`, `:135`); item 7 gives the reason.
3. **The marker's `capacity` is ignored for admission.** It is still accepted and validated as A2 item 7 states, so old and new clients keep working. A mismatch is never silent:
   - When a request's `capacity` differs from the governing value, the marker response (200 admitted and 409 queued) carries an additive warning naming the request's value, the governing value and its source (`declaration`, `fishhawkd_default` or `default`).
   - `fishhawk_doctor`'s `host_capacity` rung reports the server's governing value and source for this process's label beside the process's own value, and warns when they differ or when the last heartbeat dropped its declaration, through an additive read the implementing change names in `docs/api/v0.openapi.yaml`. Today that rung is computed locally (`backend/internal/mcpserver/onboard.go:304-336`).
4. **A2 item 2's "a client cannot resize another client's capacity" (C3) still holds.** Only the label's declarer in that account sizes it; no session, `/mcp` route, or dispatcher token for another label or account can. A `write:runs` caller can still choose a label (Q1's label residual stands), but not that label's capacity.
5. **The host implement limit.** While a fresh declaration carries a host implement limit, every request for `local-implement:<host>` uses `min(spec value, declared host implement limit)`. Otherwise it uses the spec value, as today. The spec value is what `stage_concurrency.go` resolves now: the bare shorthand `{limit: N}` (`:275-276`), or else `DefaultLimit` 1 (`:255-257`, `:371`).
   - The host limit is therefore a ceiling: it never raises a stage above its spec value.
   - The spec grammar, both validators and captured specs are unchanged. Every captured spec parses, and it keeps its meaning whenever no declaration is fresh.
   - A valid `host_implement_limit` applies independently of whether `host_capacity` is present. Omitting either field removes that field's previous declaration; an invalid supplied value clears both fields under item 1.
   - There is no fishhawkd default for this limit.
6. **ADR-100 (#4403).** In modes `off` and `shadow`, the governing capacity above is the integer limit that governs (ADR-100 D1). In `enforce`, ADR-100 D4's stored, fenced controller value governs an `auto` capacity group within `[min, max]`, unchanged. The declaration is the integer `limit` of ADR-100 D1's `min ≤ limit ≤ max`, so in `enforce` the ceiling is the declared `max`, not the integer. ADR-100's host `auto` bounds (`FISHHAWK_HOST_CAPACITY_AUTO`) are expected to take the same declarer; that is decided when they are built. Item 5's `min()` governs `local-implement:<host>` in every mode: ADR-100 D2 keeps type groups nested inside capacity integer-valued. With host capacity present, only the capacity group may use the host controller. ADR-100's headroom readings are unchanged.
7. **Hosted direction.** This is not designed here. Exact account matching is chosen over ADR-104 A2's visibility rule, under which untenanted rows are visible to every account (`backend/internal/dispatcherheartbeat/store.go:7-12`), because in hosted mode an untenanted declaration must never govern a tenanted run. On the local stack both are untenanted (`backend/cmd/fishhawkd/token.go:125-129`), so the declaration governs. In ADR-057 Mode 2, a declaration governs a workspace's admission only when it comes from a host credential registered to that workspace (ADR-057 A1). An unregistered label gets the fishhawkd default. Registration is left to the hosted design. Today a tenanted dispatcher token is representable but no route mints one (`backend/internal/postgres/migrations/0121_host_dispatcher_heartbeats.up.sql:11-21`). Under ADR-138 D4 no agent runs on the hosted node, so fishhawkd cannot measure a dispatch host. The declarer is that host's only sizing source.
8. **What this replaces.** A2 item 2's per-request limit ("Nothing is stored …") and its "Consequence" bullet, and Q1's "client-supplied" for the capacity value. D4's "The most recent declaration governs" returns, narrowed to the declarer's fresh declaration. A2 item 2's key, holders and D6 bullet stand.

**Consequences.**
- **No migration.** The declaration rides the 0121 heartbeat row. A reverted binary's payload decode ignores the extra keys (`backend/internal/dispatcherheartbeat/store.go:146`).
- **Upgrade order.** Upgrade fishhawkd and set its default first, then the dispatcher. A new dispatcher's fields are unknown to an older fishhawkd, which refuses the whole heartbeat (`host_dispatcher_heartbeat.go:60`). Until the dispatcher is upgraded, the fishhawkd default governs.
- **Rollback.** Revert the dispatcher first, then fishhawkd; per-request capacity then resumes. A reverted fishhawkd refuses a heartbeat that still carries the new fields (`host_dispatcher_heartbeat.go:60`), so campaign status reads `host_dispatcher_absent` until the dispatcher is reverted too. Remove any `--default-host-capacity` / `FISHHAWKD_DEFAULT_HOST_CAPACITY` setting before reverting fishhawkd. Keep `FISHHAWK_HOST_CAPACITY` identical in every process until the rollback window closes; #5592's interim covers the stdio processes. Retain migration 0121 and its rows.
- **Surfaces updated in the implementing change:** `backend/internal/concurrency/README.md:317-325`, `docs/api/v0.openapi.yaml` (marker warning, heartbeat fields, the doctor's read), the doctor rung schema, the `.env.example` capacity guidance, `scripts/dev`, `scripts/README.md` and the fishhawkd flag reference.
- **ADR-104 Amendment A2 item 7.** Its "no gate reads it" is narrowed by ADR-104 Amendment A3 item 2.

**Risks and residuals.**
- **Host-label drift.** The label stays client-supplied. A process with the wrong label (#5592) admits in another label's groups entirely. The #5592 shim fix stays an interim fix, and label drift stays a residual.
- **Fallback flips.** When the dispatcher stops, the fishhawkd default governs within about three intervals; if the values differ, the governing value flips. `scripts/dev` setting both from `.env` mitigates this locally. The host implement limit has no fallback: when the declaration goes stale, `local-implement:<host>` returns to the spec value, which can be higher. Holders admitted under either value keep holding.
- **Freshness ceiling.** `StaleAfter` clamps the interval at 300 s, so a stopped dispatcher's declaration can govern for up to 15 minutes.
- **One label, two machines.** Two machines under one label overwrite each other's declaration, latest wins. The row records `token_id`.
- **Tenancy mismatch.** Tenanted runs with an untenanted dispatcher token get the fishhawkd default, and the doctor rung shows it.
- **A2 item 6's gap.** Under `min()` a host implement limit only lowers, so the named shorthand still cannot run N implements.
- **Out of scope.** Cross-account budgets for one physical host (accounting is per account), and counting fishhawkd review sessions or specialist jobs against capacity.
