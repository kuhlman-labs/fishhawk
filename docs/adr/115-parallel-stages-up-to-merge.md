---
id: ADR-115
title: "Every stage runs in parallel up to the merge: the merge queue is the only serial point, capacity is the only bound, and a moving base is reconciled at merge time"
status: accepted
date: 2026-10-22
issue: https://github.com/kuhlman-labs/fishhawk/issues/4646
supersedes: []
superseded_by: []
amends: ["ADR-063", "ADR-092", "ADR-093", "ADR-094", "ADR-112"]
amended_by: ["ADR-116"]
applies_to: []
---

# ADR-115: Every stage runs in parallel up to the merge: the merge queue is the only serial point, capacity is the only bound, and a moving base is reconciled at merge time

**Status: accepted (v2.3, ratified by the Captain 2026-10-22).**
- **Origin:** the Captain's ruling, 2026-10-22: "the product should be able to run every stage in parallel up until it's ready to merge".
- **Review history:**
  - v1: fable and Opus substituting for Codex during its quota outage, both approve_with_concerns, with 4 HIGH each.
  - v2 folds in every finding. It also splits each decision into a PRODUCT mechanism and THIS REPOSITORY's declaration (ADR-110), at the Captain's request that it generalize to other codebases.
  - v2 confirmation: both approve_with_concerns. v2.1 folds in every finding:
    - integer slots with atomic group sets, instead of weighted reservation (ADR-094 Q4 unchanged);
    - the D5 resolution row is bound to a pass verdict before it counts;
    - the `stage_live` yield is specified as an R3 extension with its own clear;
    - `verify.concurrency` gets a schema home and is honoured as `shared` only on the container path;
    - the per-stage environment has an allocator and an allow-list rung;
    - the amends list is corrected.
  - v2.3: the Captain ratified with a measurement gate on D5 and on the overlap guard's removal (2026-10-22).
  - v2.2: the Captain chose D5 option D (2026-10-22). Approval transfers for union and generated resolutions only; a renumber resolution is mechanical but needs a human re-approval.
- **Freeze:** tracker-only until the 2026-11-01 catch-rate re-pin; the in-repo record follows the upkeep rule.

## Context

Fishhawk runs plan, implement, fix-up, review, acceptance and verify-only stages, decomposition slices, and many runs against one repository. An inventory found 21 serialization points, each verified in code, and the v1 reviews found more. They fall into five classes:

**1. Serial product mechanisms.**
- The per-repository verify lock (ADR-063): a second runner waits 600 s, retries, and then fails category C.
- Shipped admission defaults of 1 for `local-implement` and `local-verify` (`backend/internal/concurrency/concurrency.go`). `local-verify` is keyed per HOST (ADR-092 D3), so merge queues serialize across repositories.
- `run_stage`, `drive_run` and `run_children` bypass admission (#3969).
- The merge overlap guard refuses a merge while an implement whose scope intersects the PR is live (`merge_overlap_guard.go`).

**2. Shared mutable state.**
- The runner writes the author identity with `git config` in a lineage worktree. That lands in the SHARED `.git/config`, because worktrees share it.
- `refs/stash` is shared, and `popStash` pops the top entry.
- Ref, `packed-refs` and worktree-registry locks are shared across concurrent fetches, with no retry.
- Git auto-maintenance runs in the shared repository.
- Acceptance uses one preview target (#4019).
- The host Go build-cache trim refuses while any build is live.
- `bin/fishhawk-runner` is respawned per dispatch.
- A legacy `/tmp` handoff fallback remains.
- `dispatch_stage` on a decomposition child does not isolate its lineage.

**3. Moving-base failures.** `ErrBaseRebaseConflict` is an uncommitted stash-pop conflict at commit time on the UNPINNED arms (fix-ups, degrade, resume). It ends in category B.

**4. Merge-time conflict volume.** Parallel branches collide on append-only registries, generated files and migration numbers. A conflict ejects the queue entry (ADR-093 D4) and approval does not transfer.

**5. Operability.**
- `scripts/dev reload` and `post-merge` refuse while runners are live.
- `reload` rebuilds between stop and start, so the outage exceeds the runner's ~90 s terminal-egress budget.
- Review rounds and executor agents share provider quota without admission (#4242).
- The host-load rule reclassifies a verify that fails above 4× the core count to category C.

## Options

- **A. Status quo plus raised limits.** Rejected: without one admission path and private state, raised limits oversubscribe the host and corrupt shared git state.
- **B. Parallel up to merge; the merge queue is the only serial point; declared capacity is the only bound (recommended).**
- **C. Parallel including merge (merge trains).** Deferred: Direction T.

## Recommendation (decided now)

**Option B.** Each decision states the PRODUCT mechanism first, then what THIS REPOSITORY declares.

**D1 — The principle.**
- Every stage of every run, every decomposition slice, and every run on a repository may run concurrently, up to the point where a change is ready to merge.
- The merge is serialized only by the ADR-093 merge queue, per (repository, base). Queues for different repositories or bases are independent.
- Within ONE run, stages keep their dependency order. That is a dependency, not serialization.
- Correctness never requires serialization: shared state is made private (D2).
- Concurrency is bounded only by declared CAPACITY (D3).
- **Waiting for capacity is a wait.** Admission prevents overload; it does not convert it into failure.
  - An exhaustion that admission cannot prevent (a provider 429 mid-stage, an overloaded host) is a RETRYABLE category C with automatic re-dispatch, bounded by the stage deadline. It is never category B.

**D2 — No shared mutable state between concurrent stages (product).**
- **Git identity:** set per command with `-c user.name/-c user.email` or `GIT_AUTHOR_*` / `GIT_COMMITTER_*`. Never `git config` in a worktree (today's writes land in the shared `.git/config`).
- **Git refs:** lock contention on refs, `packed-refs` and the worktree registry is a bounded retry (a wait), not a failure.
- **Git maintenance:** every runner git child runs with `maintenance.auto=false` and `gc.auto=0`. Shared-repository maintenance runs only from an admitted housekeeping job.
- **Stash:** the shared `refs/stash` stack is never used. A pre-commit snapshot is a private-index commit under `refs/fishhawk/stash/<stage>`. It includes untracked files but excludes ignored ones, is restored by SHA, and is deleted at stage end.
- **Decomposition children:** `dispatch_stage` on a child isolates its lineage as `run_children` does, or refuses.
- **Per-stage environment:**
  - **Allocation.** The RUNNER, which can see the host, allocates each stage a port range from a host-local allocator, and a namespace: a stage-unique token that repository commands use to prefix databases, containers and files.
  - **Exported variables:** `FISHHAWK_STAGE_PORT_BASE`, `FISHHAWK_STAGE_NAMESPACE`, and the existing `FISHHAWK_STAGE_ID`, reused with its current meaning. They are added as an explicit rung of the gate and agent environment allow-lists.
  - **Opt-in.** A repository declares that its preview and acceptance commands consume them. Undeclared, those stages hold a repository-scoped exclusive group (conservative default).
- **Runner binary:** each stage dispatch executes a content-addressed snapshot of the runner, recorded on the stage. The runner keeps N-1 wire compatibility with the backend.
- **The `/tmp` handoff:** the legacy fixed-path fallback (`/tmp/fishhawk-acceptance.json`) is removed.
- **The verify gate:**
  - Where a gate image is configured, a runner's gate verify runs on the container path (ADR-063), with private services (ADR-088 `services:`), lint cache and lock.
  - The `clone` path is a DEGRADED mode that `fishhawk doctor` warns about. On it, the verify lock is a capacity wait bounded by the stage deadline and stated in the `docs/spec/` verify contract (ADR-110 rule 5); exit 73 still means "not judged".
  - Still shared even on the container path, and counted as capacity: the container runtime's CPU, memory and disk, the image store, and the module-cache read seed.
- **Verify concurrency declaration.** Workflow-v2 `executor.verify` gains an optional `concurrency: shared | exclusive`. It is an additive change through the spec-change checklist, reusing ADR-100 D2's vocabulary. Rules:
  - **Default is `exclusive`.**
  - **`shared` is honoured only when the runner reports the container gate path** on its dispatch marker. On `clone`, a stage is treated as `exclusive` whatever it declares.
  - **`exclusive` adds a repository-scoped verify group of 1** to the stage's group set, for the WHOLE stage. Stated consequence: on a clone-path host, a repository's implements serialize.
  - **The merge-queue pass** uses the implement stage's value, since it reuses that stage's verify command. If two implement stages disagree, the strictest wins.
  - **A mid-stage fallback** from container to clone, after admission, re-queues the stage. It never acquires a second time.
- **This repository:**
  - declares `concurrency: shared` on its implement stages' `executor.verify`;
  - declares that its preview and acceptance commands consume the per-stage environment (#4019);
  - keys its shell verify lock per worktree;
  - makes its Go build-cache trim safe beside live builds (age floor > the longest build + Go's 1 h mtime refresh).
  - The gate image and `FISHHAWK_GATE_SERVICES=postgres` are this host's RUNNER environment, provided by the operator, not a repository declaration.

**D3 — Declared capacity is the only bound (amends ADR-092 D3 and ADR-094 Q2; ADR-094 Q4 is UNCHANGED).**
- **One admission path:** ADR-094 D1 (#3969) is a prerequisite. No verb spawns work outside admission.
- **Integer slots, atomic group sets.** Admission stays integer slots (ADR-094 Q4, re-affirmed by ADR-100 D7). There are no weights.
  - Hold-and-wait is avoided by ADR-094 D2: a stage acquires its WHOLE group set atomically at admission, including its host capacity slot and, where `exclusive`, its repository verify group, and never acquires again.
  - A gate verify inside a stage runs under that stage's slots.
- **Host capacity** is declared by the host (`FISHHAWK_HOST_CAPACITY`, ADR-094 Q1). `fishhawk doctor` SUGGESTS a value from the container runtime's allotment (on macOS, the Docker VM, not host cores).
  - Honestly, verify-bearing parallelism on a laptop is about 1–2; a larger host scales.
- **Who governs in `enforce`:** in ADR-100's `enforce` mode, its headroom controller sets each group's limit within `[min, max]`, and admission fills that limit with integer slots. Rollout follows ADR-100's shadow → canary → enforce staging under Captain gates.
- **Queue passes.** At least one host capacity unit is RESERVED for merge-queue verify passes, so the only serial point never waits behind implements. This amends ADR-094 Q2's strict FIFO.
- **Verify grouping** is ADR-094 D5's already-decided `repository:<repo>:verify:<base>`, replacing ADR-092 D3's per-host `local-verify` limit of 1.
- **Per-type defaults.** The shipped per-type default limits of 1 are removed. A declared `concurrency: {limit: N}` stays valid as an explicit per-type cap.
- **Provider-account admission** (ADR-094 D6, #4242) covers review rounds AND executor agents. Exhausted quota parks queued work until the reset. In-flight exhaustion follows D1, and a review round past its budget is `reviewer_unavailable` as ADR-094 D6 states.
- **Host load.** The host-load rule stays keyed on MEASURED load, so orphan load (#3663) is still identified. Its outcome becomes a retryable category C with automatic re-dispatch (D1), not a terminal C.

**D4 — A moving base is reconciled at merge time (amends ADR-093 and ADR-112 R3).**
- **Pinning.** Every commit-time path is pinned to its dispatch-time base or head:
  - implements (#3973);
  - fix-ups;
  - the degrade path;
  - the held-commit resume;
  - a decomposition child whose wave base was absent at dispatch.
  The base is persisted on the stage so a resume reads it. The fresh-fetch arm and `ErrBaseRebaseConflict` are REMOVED, not rerouted. Conflicts surface only at the queue advance (ADR-093 D4 → the #3202 conflict pass). A conflict past the conflict-pass ceiling parks the run for replan, never category B.
- **"Live"** means an agent-executing stage of the run: implement, fix-up, acceptance or conflict resolution. The queue's own verify-only pass is excluded.
- **The `stage_live` yield extends the ADR-112 R3 marker** with a PRE-ADVANCE site and a SETTLE-driven clear:
  - **Queued rows:** `AdmitNext` skips a row whose run has a live stage, with no marker.
  - **Admitted rows:** an admitted row that is not yet advancing yields with `yield_reason: stage_live`.
  - **Clearing:** it clears on the stage's terminal event, not on an approval fold, and re-anchors to the run's current head (the ADR-092 re-anchor). The clear has its own predicate. It is not the approval compare-and-swap, and it is not subject to the approval re-probe's oldest-first ordering, so it cannot starve approval-yielded rows.
  - **Remedy:** `next_step: await_stage`.
  - **Approval wait:** a `stage_live` episode ends any approval-wait episode. On return at a new head, a fresh approval-wait episode starts with a new `approval_wait_started_at`, since a fix-up push dismisses approvals.
    - **Contrast with other pushes:** a push to a yielded row that comes from no agent-executing stage only re-anchors within the SAME episode (the #4632 C2 rule), so arbitrary pushes cannot restart the window.
    - **Bound:** fresh episodes come only from agent-executing stages, so the fix-up ceiling caps how many there can be.
- **Dispatch side.** Dispatching a fix-up or acceptance stage waits only while the run's entry is mid-advance, mid-verify-pass or merging. During R3's approval wait it does not wait: the entry yields `stage_live`. Transitions are recorded under the admission-epoch compare-and-swap.
- **Coordination.** #4632 implements R3 as ratified in ADR-112. The `stage_live` reason, its `next_step` and its columns land later, with their own migration.
- **The merge overlap guard** stays until fix-up pinning AND D5 land. It then becomes advisory evidence on the verdict, with the measured conflict-pass rate.
  - **Cost, stated:** without D5, an overlap becomes a merge-time conflict, and a conflict-resolution advance does not transfer approval (ADR-093 Q1(a)).

**D5 — Mechanical resolution of declared conflicts (product; amends ADR-112 R1).**
- **Where it runs.** Resolution runs in a RUNNER pass on the gate path (the #3202 pass shape), never in fishhawkd, because it executes repository-declared commands (ADR-063). `rebase_run_branch` never writes a resolution row.
- **Declarations** live in `.fishhawk/workflows.yaml` (an additive workflow-v2 change). They are read from the BASE tip, never the PR head, and the declaration file can never itself be a declared path. A repository may declare:
  - **union paths:** one entry per line, order-insensitive. A hunk qualifies only if BOTH sides only INSERT lines. Any modification or deletion is a real conflict.
  - **generated paths:** with a regenerate command and its declared input paths.
  - **renumber rules:** with a command that renumbers AND rewrites every reference to the number.
- **A new CLEAN variant (ADR-112 R1(a)):**
  - The pushed commit's parents are exactly (prior_head, base_tip), confirmed by fishhawkd through a forge commit read.
  - Its tree equals git's three-way merge tree everywhere except the declared paths, where it equals the declared resolution.
  - The row is recorded as `branch_rebased` with `resolution: union | generated | renumber`, the runner-pushed commit SHA in place of a forge `merge_commit_sha`, the declaration's base-tip blob SHA, the resolved paths and the command digests.
  - **Binding:** the row is NOT CLEAN, and is ineligible for the R1 chain, until a verify-only pass verdict bound to that exact `new_head_sha` records `recompute: match`. That pass is a separate gate execution, which recomputes the expected tree from the two parents and the declaration, and re-runs the generated and renumber commands for determinism.
  - **Generators:** a non-deterministic generator is a conflict (eject to conflict resolution), never a transfer.
  - **No transfer when the PR changes a declared command or its declared input paths.** The resolution then follows ADR-093 D4.
- **Approval transfer, by kind (the Captain's choice, option D).**
  - **Union and generated resolutions** carry the run's approval forward under this CLEAN variant. That approval then covers content no human saw: the union lines and the regenerated output. That is a larger step than ADR-093 Q1(a), and this ADR ratifies it as such.
    - **Bounds:** the narrow union definition, the base-tip declaration and generator, the separate recomputation, and the green pass.
    - **Residual, stated:** recomputation is a separate gate execution on the same host and runner class, not a different trust domain.
  - **Renumber resolutions** are applied mechanically and verified the same way, but approval does NOT transfer. The entry yields with `next_step: approve_pr` (ADR-112 R3), because a text rewrite of references can change meaning. A repository can avoid renumbering by using collision-free migration identifiers.
  - **Visibility:** every mechanical resolution, of any kind, posts its resolution diff to the PR and is recorded in the `branch_rebased` row.
- **Rejected options,** recorded:
  - A, eject every conflict to the agent conflict pass;
  - B, mechanical resolution without any transfer;
  - C, transfer for all three kinds.
- **What the pass may not catch:**
  - duplicate entries in unkeyed registries;
  - duplicate OpenAPI `operationId`s or path keys, where the repository's lint is not in its verify;
  - same-string registry entries with different meanings.
  A repository that cannot accept this declares no union paths for such files.
- **GitLab:** `provider_unsupported` here, as in ADR-112 R6.
- **This repository** declares:
  - **union:** the line-per-entry registries only (for example the audit-category list, if restructured to one entry per line);
  - **generated:** `docs/adr/index.json` (`scripts/check-adr --write-index`), the site reference (`scripts/gen-site-reference`), and the schema mirrors (`scripts/sync-schemas`), each with its input paths;
  - **renumber:** migrations.
  - Structured documents (the OpenAPI YAML, the server README, ARCHITECTURE.md) are NOT union paths. They are restructured to one file per entry where they collide, or left to conflict resolution.
  - Its `basedelta` leg table and `wirecontract` manifest stay in ADR-096's GlobalSurface, so no verdict transfer applies to them.
  - These declarations are operator-authored.

**D6 — Operability under constant load (product, then this repository).**
- **Product:**
  - a fishhawkd restart is bounded and survivable;
  - runner non-terminal calls (prompt fetch, heartbeat, the scope-amendment channel) get a bounded outage retry, not the ~3.5 s blip budget;
  - admission waiters keep their queue position across a restart: the waiter TTL gains a restart grace at least as long as the terminal-egress budget, so a ~90 s outage does not reset waiters to the tail;
  - every detached worker (queue pumps, review goroutines, sweeps) is inventoried, each with a restart test;
  - gating review rounds remain restart BLOCKERS, as decided by ADR-091 (which this ADR does not amend).
- **This repository:**
  - `scripts/dev reload` builds BEFORE stopping, so the outage is the daemon restart plus migrations, below the terminal-egress budget;
  - once the product items land, the runner-live refusal becomes a warning, and only genuinely lossy blockers (gating rounds, undispatched children in `post-merge`) still refuse;
  - the one-implement-at-a-time and `run_children max_parallel: 1` conventions are retired when D3 reaches enforce.

**D7 — The merge queue stays serial** per (repository, base): ADR-093 ordering, merge-on-pass, and one verify per merge at the head of the line. Its throughput becomes the measured bottleneck once D1–D6 land.

## Directions (not decided here)

**Direction T — Merge trains:** batched or speculative verifies of several queue entries, adopted only if the queue is measured as the bottleneck.

## Decision

**Accepted 2026-10-22** by the Captain (repository maintainer): **Option B**, i.e. D1–D7 as stated in the Recommendation (v2.3). D5 takes option D: approval transfers for union and generated resolutions only. The implementation is staged by the measurement gate in Consequences. Direction T is NOT decided. It amends ADR-063, ADR-092 D3, ADR-093, ADR-094 Q2, and ADR-112 R1 and R3, as listed under Amended clauses.

## Consequences

- **Gains:**
  - runs, stages and slices proceed concurrently up to the merge queue, with no shared-state corruption;
  - capacity waits replace contention failures;
  - merge queues for different repositories no longer serialize each other;
  - declared mechanical conflicts stop costing a human re-review;
  - deploys need no quiet window except for gating rounds;
  - every mechanism is repository-agnostic, with this repository as a configured instance.
- **Costs:**
  - D2's git, stash, environment, snapshot and gate work;
  - #3969, #4240, #4241, #4242 and executor admission;
  - staged enforcement under ADR-100;
  - the queue-pass reserve, the verify re-keying and the `executor.verify.concurrency` schema change;
  - commit-time pinning and the two-way yield;
  - the D5 runner pass, the CLEAN variant, tree recomputation, the workflow-v2 declarations and their portability fixture;
  - the D6 restart work;
  - this repository's declarations and registry restructuring.
- **Residuals:**
  - laptop parallelism is small (about 1–2 verify-bearing stages);
  - D5 transfers approval to mechanically produced union and generated content, with the named blind spots; a renumber resolution still needs a human approval;
  - the database pool (MaxConns 10) and audit-append serialization are contention points to be measured;
  - gating review rounds still block a restart;
  - merge-queue throughput bounds end-to-end latency until Direction T.
- **Implementation order:**
  1. D2 correctness: git identity, ref retry, maintenance, stash, child isolation, `/tmp`, per-stage runner snapshot.
  2. #3969 (one admission path).
  3. #4240 and #4241 (atomic group sets, host capacity), the queue-pass reserve, the verify re-keying, `executor.verify.concurrency`, and the gate-path report on the dispatch marker; shadow → canary → enforce.
  4. The per-stage environment and #4019; #4242 and executor admission.
  5. D4 pinning and the two-way yield.
  6. D5: runner pass, CLEAN variant, declarations, portability fixture.
  7. The overlap guard becomes advisory.
  8. D6.

  The operator's one-implement convention holds until step 3 reaches enforce.
- **Measurement gate (ratified with this ADR).**
  - Steps 6 (D5) and 7 (overlap guard advisory) start only after the merge-queue conflict rate has been MEASURED under real parallelism, after step 3 reaches enforce, with the result recorded on their issues.
  - Steps 1–2 and D4's pinning are built first, because they fix live correctness and failure-mode problems at any parallelism.
- **Portability:** a non-Fishhawk fixture repository runs two concurrent implements and one D5 union advance through the production code (ADR-110 rule 3(a)).
- **Amended clauses:**
  - ADR-063: container gate path required where an image is configured; the clone-path lock becomes a bounded capacity wait;
  - ADR-092 D3: per-host verify limit;
  - ADR-093: overlap guard advisory; `stage_live` yield;
  - ADR-094 Q2: queue-pass reserve;
  - ADR-112 R1 (D5 CLEAN variant) and R3 (`stage_live` yield).
- **Record metadata.** amends: ["ADR-063", "ADR-092", "ADR-093", "ADR-094", "ADR-112"]; supersedes: []; applies_to: []
- **Related:** ADR-027, ADR-031, ADR-087, ADR-088, ADR-090, ADR-091, ADR-100, ADR-109, ADR-096, ADR-098, ADR-110, ADR-111, ADR-113, #2897, #3086, #3202, #3901, #3969, #3973, #4019, #4077, #4083, #4119, #4240–#4243, #4582.
