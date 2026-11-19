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
6. **A repository spec cannot set a host-scoped group's limit.** A host-scoped group is shared by EVERY repository dispatching on that host, so it is a cross-repository trust boundary: one repository's spec must not widen or narrow a resource every other repository on the host competes for. A `concurrency.groups` entry with scope `host` therefore carries no `limit`, for any name; both validators refuse it. The host's legacy groups keep the host defaults (`local-implement:<host>` and `local-verify:<host>`), and any other host-scoped group is resolved at limit 1. Host-level limits are the host operator's to declare under D4 (#4241). Repository-scoped entries keep their 1..64 limit. The single-group shorthand `concurrency: {limit: N}` is unchanged, because single-group behaviour must stay byte-identical.

**Consequences.**
- No key string, advisory-lock key or persisted row is rewritten across the deploy.
- The binary and the schema change together. Stop every old fishhawkd, including every API replica, migrate up, then start the new binary; a rolling upgrade is unsupported for this release, because each binary's `ON CONFLICT` target needs its own primary key. Rollback is the mirror image: stop the new binary, migrate down, start the reverted binary.
- The down migration fails closed. It refuses, naming the stage, while any stage holds more than one `held` row, because collapsing a live multi-group hold would make the dropped groups look free; drain those stages first. A queued-only multi-row stage collapses to its canonical-first row and restarts at the tail on its next poll.
- Before rolling back, confirm that no non-terminal run's captured workflow spec declares `concurrency.groups`, or cancel those runs: a reverted binary cannot parse such a spec and falls back to the default group.
- Store-side, a group set may hold up to 16 groups, above the schema's 8 entries, so #4241 can add the host capacity group to a full declared set without a store change.
- The read surfaces gain an additive `groups[]` list (D7's first step); their existing top-level fields describe the binding group.

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
