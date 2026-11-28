---
id: ADR-150
title: "Speculative merge queue: each entry verifies on the base plus every entry ahead of it"
status: accepted
date: 2026-11-28
issue: https://github.com/kuhlman-labs/fishhawk/issues/5641
supersedes: []
superseded_by: []
amends: ["ADR-090", "ADR-092", "ADR-093", "ADR-109", "ADR-112", "ADR-115", "ADR-142"]
applies_to: []
---

# ADR-150: Speculative merge queue: each entry verifies on the base plus every entry ahead of it

**Status: accepted (ratified 2026-11-28).**

## Context

The Fishhawk-owned merge queue (ADR-093) merges one entry per `(repository, base)` at a time.
- **Head-of-line cycle.** The head of the line is advanced to the base tip by a forge-side merge of the base INTO the PR branch (`backend/internal/server/rebase_branch.go:658`). It is verified at that new head, then merged through the forge API pinned to it (`backend/internal/server/merge_queue_merge.go:1225`).
- **Serial by construction.**
  - A per-base advisory lock admits at most one `live` entry (`backend/internal/mergequeue/store.go:426`, `:466-487`).
  - ADR-092 D1 allows one live pass per base (`docs/adr/092-serialize-merge-candidate-verify-passes.md:52-55`).
  - ADR-093 D9 fixed batch size 1 (`docs/adr/093-fishhawk-owned-merge-queue.md:113`), and ADR-115 D7 kept the queue serial (`docs/adr/115-parallel-stages-up-to-merge.md:196`).
  - Speculative verification was deferred twice: ADR-093 Option D, and ADR-115 Direction T.
- **What the next entry pays.** Every merge leaves the next entry behind. Its turn then pays for:
  - a new advance push to its PR branch;
  - the forge's required checks at that new head, which merge-on-pass must see green (`backend/internal/server/merge_queue_eligibility.go:314-316`);
  - a fresh trusted forge approval at the advanced head wherever the forge dismisses stale approvals (ADR-112 R1);
  - a verify-only pass at that head.
  ADR-096 can skip the pass on an independent base delta. It cannot skip the push, the checks or the approval.

Other merge queues test entry *k* against the base plus every entry ahead of it, so entry *k* is ready to land the moment the entries ahead land. Examples are GitHub's merge queue, GitLab merge trains, Bors and Mergify. The Captain directed this behaviour for Fishhawk on 2026-11-28.

ADR-093 Q4 tied reopening Option D to a measured median queue wait above two hours (`docs/adr/093-fishhawk-owned-merge-queue.md:123`). Nothing measures that wait today. The read surface reports only admission-to-merge turns (`backend/internal/mergequeue/store.go:1285`, `backend/internal/server/merge_queue_read.go:443`). This record adopts the behaviour on the Captain's direction instead of that trigger, and adds the measurement.

Four facts of the current code bound the design:
- **The verify pass fetches a branch by name** and refuses a tip other than its anchored SHA (`runner/cmd/fishhawk-runner/mergecandidateverify.go:192-204`). Its request carries only branch, expected head and cause (`runner/cmd/fishhawk-runner/mergecandidateverify.go:32-36`).
- **A pass's result depends on history as well as the tree.**
  - This repository's verify command diffs against `merge-base(FISHHAWK_DIFF_BASE, HEAD)`, defaulting to `origin/main` (`scripts/verify-change:10-11`).
  - The gate environment is default-deny (`runner/cmd/fishhawk-runner/gateenv.go:232-240`).
  - The trigger carries no base SHA (`backend/internal/server/merge_candidate_verify.go:151-163`).
- **One verify pass runs per host.** Passes keep the host verify group (`docs/adr/094-resource-keyed-admission.md:191`), whose limit is the constant 1 (`backend/internal/concurrency/concurrency.go:46`, `backend/internal/server/stage_concurrency.go:252`).
- **This repository's CI runs only for pull requests to, and pushes to, `main`** (`.github/workflows/ci.yml:3-7`).

## Options

- **A. Keep the serial queue, with ADR-096 transfer.** No new machinery. Each landing keeps its advance push, its checks and, under stale-approval dismissal, a human re-approval.
- **B. A speculative train with one pass per position, landing each PR at its unchanged head (recommended).**
  - Fishhawk builds candidate *k* on a Fishhawk-owned branch as candidate *k−1* plus entry *k*, and verifies it.
  - Each PR is merged through the forge at its own head, only after the forge's test merge is shown to have exactly the verified tree.
  - Because a pass covers exactly one position, attribution is direct once the positions ahead have passed. No bisection is needed.
- **C. Batched speculation with bisection** (ADR-093 Option D). It needs fewer passes when green, but attribution machinery and more wasted passes on red. Deferred.
- **D. Fast-forward the base to the verified candidate**, as GitHub's merge queue does.
  - The landed commit is the verified commit, the update is atomic, and the forge's checks can run on the candidate.
  - It requires Fishhawk to be a bypass actor on the protected base, replaces the merge method with merge commits, and needs CI to run on candidate branches. Deferred to a direction.
- **E. Stack PR branches** (push candidate *k* to PR *k*'s branch). Every rebuild rewrites a PR branch, which dismisses approvals and makes each PR's diff show the entries ahead. Rejected.
- **F. Forge-native queues.** Rejected twice already (ADR-093 Context); unchanged.

## Recommendation

Adopt Option B with D1–D10.

**D1. Depth.**
- A repository declares `merge_queue: {depth: N}` at the workflow document root, beside `merge_method`. N is an integer from 1 to 8; absent means 1.
- Effective depth 1 is exactly the ADR-093 cycle as amended, so nothing changes for a repository that does not declare a depth.
- Declaring N > 1 is the repository's acceptance of two things:
  - D9's changed guarantee;
  - publishing combined candidate content in its base repository (D2).
- The effective depth is 1, with the reason named on the queue read surface, when any of these holds:
  - the declared `merge_method` is `rebase`;
  - the forge is not GitHub (GitLab follows when its merge-ref read is built, as ADR-093 D9 already defers GitLab);
  - the base requires branches to be up to date: classic protection `required_status_checks.strict`, a ruleset's `strict_required_status_checks_policy`, or a forge-native merge-queue rule;
  - that protection or those rulesets cannot be read (fail closed);
  - the most recent candidate pass for the repository came from a runner without D3's capability. That pass reports not-executed with cause `capability_absent`, and the clamp lifts when a runner advertising the capability completes any verify-only pass for the repository, including a depth-1 pass;
  - the base is stopped under D6.
- A change in any of these inputs rebuilds the train (D4).

**D2. Candidates.**
- For each `(repository, base)`, the first *d* entries in queue order form the train, where *d* is the effective depth. Each entry must meet ADR-093 D1's eligibility except "behind", and ADR-112 R1/R2 evaluated at its head Hₖ. Queue order is ADR-093 D2's order, with D6's holds.
- Position *k* holds a candidate with an immutable identity, consisting of:
  - Cₖ₋₁;
  - Hₖ, named by SHA and anchored when the entry joined;
  - Cₖ, a merge commit whose parents are exactly (Cₖ₋₁, Hₖ).

  C₀ is the base tip B the train was built on.
- Fishhawk builds Cₖ by creating the branch `fishhawk-queue/<base>/<candidate id>` at Cₖ₋₁ and merging Hₖ into it through the forge's `MergeBranch` (`backend/internal/githubclient/client.go:2705`). It then checks the parent pair, as ADR-112 R1(a)(ii) does for an advance.
- **Branch names are never reused.** A candidate branch is deleted when its candidate lands, is invalidated or its entry settles. A stale build or cleanup therefore cannot touch a replacement.
- **Ownership.** Candidate branches are controller-owned artifacts, separate from run lineage.
  - Fishhawk never directs an agent to write one, and candidate evidence is never run-authorship evidence.
  - Nothing here gives forge-level write exclusion: any repository writer can push `fishhawk-queue/**`, and an operator may restrict the namespace with a ruleset. The controls are the parent check, D3's expected-SHA check and D5's tree checks.
- **Push workflows.** A repository whose workflows trigger on pushes matching these branches runs them on combined content, with their configured privileges. No CI change is decided here.
- A member whose head is not a branch of the base repository never joins a train.
- **A verify command is required.** Train membership requires a nonempty declared verify command.
  - No candidate is built for, or across, an entry without one.
  - That entry keeps its queue position. When it reaches the head, it is serviced through the unchanged depth-1 cycle, and speculation is then rebuilt from the resulting base.
  - `not_required` never counts as a passed train verdict.

**D3. One pass per position.**
- Position *k* is verified by a verify-only pass on Cₖ. It runs under ADR-090 D3's runner and ADR-063 isolation, through the existing verify group, admitted in position order.
- **ADR-092 D1 for a train** becomes "at most one live pass per candidate". ADR-092 D2's re-anchor and advance never apply to a candidate pass.
- **A new runner capability** carries the candidate identity and `diff_base_sha` = Cₖ₋₁ through trigger, prompt, gate and result.
  - The runner fetches both commits.
  - It injects `FISHHAWK_DIFF_BASE` explicitly into the gate's environment.
  - It returns both anchors with its result.
  - A result with a missing or mismatched anchor is refused, and a runner without the capability cannot produce a train verdict.
- **The verdict key.** A verdict is recorded against (verify configuration, tree of Cₖ₋₁, tree of Cₖ). Another candidate may reuse it only when all three are identical.
- **Not executed.** A not-executed pass is re-triggered at the same Cₖ under ADR-090 D3's rule. It rebuilds only on `head_moved` or a fetch failure. A `capability_absent` result is not re-triggered; it clamps under D1.

**D4. Invalidation.**
- Each of these invalidates position *k* and every position behind it:
  - entry *k*'s red pass, acted on under D6;
  - its ejection, yield or withdrawal;
  - any settle of entry *k* other than its own landing (PR merged or closed outside, run terminal);
  - a change to Hₖ or to the PR's base;
  - a candidate branch that is missing or not at Cₖ.
- **Order mutations.** For every order mutation, including promotion and an entry joining ahead, the old and new order are compared under the per-base lock.
  - The suffix is invalidated from the earliest position whose entry or predecessor changes.
  - Only the unchanged prefix keeps its candidate identities and passes.
  - The order change and the invalidation commit atomically, before any further candidate admission or landing authorization.
  - Promotion moves an entry directly behind position 1 when position 1 holds a candidate, a live pass or a landing reservation; otherwise it moves the entry to the head, as ADR-093's promote does today. An entry holding a candidate may be promoted. A position 1 in any of those states is never displaced.
- A base tip whose tree differs from the current C₀'s tree invalidates every position.
- A change to the effective depth or its D1 inputs rebuilds the train.
- Positions ahead of the invalidated suffix keep their candidates and passes.
- An invalidated candidate's in-flight pass is cancelled best effort. Its result, whenever it arrives, never changes current state; only D3's identical-key reuse can carry it.

**D5. Landing, strictly in order.**
- Only position 1 lands, and only when all of these hold:
  - its pass is green;
  - ADR-093 D1's merge prerequisites and the forge's current requirements hold;
  - ADR-112 R1–R4 are evaluated before dispatch, with H₁ as R1's verified head. A recorded CLEAN chain ending at H₁ still counts.
- Existing waits and yields apply unchanged.
- For a train landing, the merge gate's verify leg is satisfied by the passed candidate at C₁. An outstanding ADR-090 D2 / ADR-142 requirement at H₁ is discharged by that pass, and by no other result that is not at H₁.
- **The pre-checks:**
  - **(a)** the base tip's tree equals C₀'s tree;
  - **(b)** the forge's test merge for the PR, GitHub's `merge_commit_sha`, has parents exactly (base tip, H₁) and C₁'s tree.
- **The order of a train landing.** A train landing is the queue path.
  1. **Outside any claim**, Fishhawk evaluates (a) and (b).
     - A test merge that is absent, still computing or built on another base tip is a held outcome repeated across pumps, as ADR-112 R3's head-of-line wait is (`docs/adr/112-queue-merge-forge-approval-predicate.md:92`). It takes no ledger claim and no reservation, and it is never held inside the merge-intent lock.
     - The wait is bounded by a persisted start per candidate, `FISHHAWKD_MERGE_QUEUE_TEST_MERGE_WAIT`, default 5 minutes.
     - Only these rebuild the train: a current test merge with another tree, a forge-reported conflict, a failed (a), or expiry of the wait.
  2. **Under the merge-intent lock, once (a) and (b) hold**, Fishhawk does these in order:
     - evaluates ADR-112 R1–R4;
     - writes `merge_queue_authorized`;
     - takes the durable per-base landing reservation;
     - claims the ADR-109 dispatch ledger;
     - re-reads (a) and (b) once.
  3. **If that re-read no longer holds**, no merge is dispatched. The claim and the reservation are released by ADR-109's release path in the same lock, and the landing returns to step 1.
  4. **Otherwise** Fishhawk merges through the forge pinned to H₁ with the declared merge method (`backend/internal/githubclient/client.go:1736`), then runs the post-check.
- **Post-check.** Fishhawk resolves the landed commit L from the merged PR's `merge_commit_sha`, for squash and merge alike. It requires that L's first parent is the tip read in (a), and that (tree of first parent, tree of L) equals (tree of C₀, tree of C₁).
  - Missing or unreadable evidence holds the reservation unresolved.
  - A later matching base tip does not discharge a mismatch at L.
- **Resolving a reservation.** A reservation resolves in exactly one of four ways:
  - the post-check passes;
  - the post-check mismatches (D6);
  - the forge refused the merge, and a fresh read shows the PR unmerged, whether it is open at any head or closed. The reservation and the claim are then released. A PR still open at H₁ has its refusal routed as D6 says; a moved head or a closed PR is invalidated under D4;
  - an operator resolves a paged reservation after reading the forge. A PR that reads unmerged releases the reservation and the claim. A PR that reads merged is treated as a post-check mismatch (D6). The act is audited as `merge_queue_train_resumed`, whose payload names the act (`reservation_resolved` or `stop_resumed`) and the forge reading (`unmerged` or `merged`).

  While a reservation is unresolved, no queue merge on that base is dispatched at any depth. Invalidation cannot release or replace it. A reservation unresolved for longer than the test-merge wait pages the operator, and is never released automatically.
- **On success**, position 2 becomes position 1, with L as its C₀ by tree identity. No pass or push is repeated, unless check (b) for the new position 1 shows another tree, which rebuilds it.

**D6. Failure, conflict and stop.**
- **A red pass at *k*** is acted on only when the current candidates at positions 1 to *k−1* have all passed. Until then it is held, and a rebuild discards it. When acted on, it ejects entry *k* exactly as ADR-093 D4 and ADR-090 D6 route a red today, and re-entry is at the tail (ADR-093 Q2).
- **A candidate merge conflict at *k*** takes entry *k* out of the train. It is not ejected: it stays held at its order position and rejoins when an entry ahead of it settles.
  - Entries behind it may land first, which amends ADR-093 D2's FIFO for this case.
  - When it reaches the head of the line with no train ahead, it runs the depth-1 cycle, where a conflict with the base ejects it as `advance_conflict`.
- **A forge refusal at landing** is handled as ADR-093 D4 and ADR-112 handle it at the head of the line.
- **A post-check mismatch** means an unverified tree may have reached the base. Fishhawk:
  - records `merge_queue_landed_tree_mismatch`;
  - pages the operator;
  - invalidates the train;
  - stops the base at effective depth 1 until an operator resumes it.

  The next depth-1 head-of-line cycle verifies on the new tip. The stop is persisted.

**D7. Persistence and serialization.**
- Candidates are rows keyed by candidate identity. Each row holds:
  - its entry, position and train revision;
  - Cₖ₋₁, Hₖ, Cₖ and both trees;
  - its state: `building`, `verifying`, `passed`, `failed`, `invalidated` or `landed`.
- The per-base advisory lock serializes candidate and train mutations. "At most one live entry per base" becomes:
  - at most *d* entries hold live candidates;
  - at most one landing reservation exists.
- Forge calls (ref create, merge, reads) run outside that lock. Their results are committed by a compare-and-swap on the admission epoch and the candidate identity, extending ADR-109's fencing. Account scoping is unchanged.
- On restart, fishhawkd does these in order:
  1. reconciles any landing reservation with the forge's actual PR state and merged commit, never blindly redispatching;
  2. restores a persisted stop;
  3. re-checks each candidate branch against Cₖ, invalidating any that are missing or moved.

**D8. Visibility and audit.**
- The queue read surface shows, per base:
  - the effective depth, and any D1 or D6 reason, including unreadable protection;
  - the train revision;
  - each position's Hₖ, Cₖ, pass state and held red;
  - the median and p90 wait from enqueue to landing over the last 20 landings. This is the measurement ADR-093 Q4 lacked.
- New audit categories, registered in `backend/internal/audit/categories.go`:
  - `merge_queue_candidate_built`;
  - `merge_queue_candidate_invalidated`, with the D4 cause;
  - `merge_queue_landed_tree_mismatch`;
  - `merge_queue_train_resumed`.

  Existing `merge_queue_verified` and `merge_queue_merged` rows gain the candidate identity and position.

**D9. The changed guarantee.**
- For a train landing, ADR-090 D1's up-to-date requirement is replaced by D5's checks. The PR merges while behind its base.
- What makes that safe is that the forge's merge result is shown, before and after the merge, to have the verified tree pair.
- **What runs on the combination.**
  - The train guarantees that the declared verify command runs on the combined candidate.
  - Fishhawk's required-check predicate still requires checks at H₁, and GitHub may additionally require checks on its current test-merge commit.
  - Matching candidate push workflows may also test the combination. This ADR neither requires nor credits their results as candidate verification.

**D10. What stays.**
- ADR-093 D2 ordering (except D6's conflict hold), the promote verb and ADR-093 D5's external-merge boundary.
- ADR-090's exact-head rule for every non-train merge.
- ADR-096 and ADR-098 transfer at effective depth 1. Neither is ever applied to a train candidate.
- The verify group and its limit. Raising the limit is not decided here.

## Decision

**Accepted 2026-11-28** by the Captain (manual ratification path; approver and hashes in the ratification comment). Adopt Option B and D1–D10, including errata E1 (the `merge_queue_train_resumed` payload) and E2 (promotion at depth 1), both reviewer-supplied and accepted by the Captain. For train landings and candidate passes only, this amends ADR-090 D1 and D2, ADR-092 D1 and D2, ADR-093 D2, D3, D9 and Q4, ADR-109's admission fencing, ADR-112 R1's verified head, ADR-115 D7 and Direction T, and ADR-142 D5. Depth is opt-in: a repository that declares no `merge_queue.depth` keeps the ADR-093 cycle unchanged. All other decisions remain as written.

## Consequences

- **Gains.**
  - Entry *k*'s pass runs while the entries ahead are verifying or landing, and a landing repeats no pass.
  - The train never pushes a PR branch. Pushes, re-runs of CI and re-approvals that today follow each advance therefore no longer happen.
  - The forge can still demand fresh checks or approvals, for example when a ruleset dismisses approvals on a base change. Those still wait or yield as today.
  - At the local verify limit of 1 passes stay serial. With a larger limit (a CI-backed runner, or a later ADR-094 amendment), they run concurrently.
- **Costs.**
  - A red pass or a rebuild discards the passes behind it: at worst *d − k* passes per failure.
  - Candidates add forge writes: one ref and one merge per position per revision.
- **Residuals.**
  - **Unpinned base.** GitHub does not pin the base between check (b) and the merge (ADR-093 D3's residual). The post-check detects an unverified landing in that window but cannot prevent it.
  - **Checks narrower than CI.** A repository whose required checks cover more than its verify command loses that coverage on the combination. Examples are code scanning, site builds and frontend checks. It should keep depth 1 until Option D.
  - **History-dependent verification.** D3 makes the diff base exact. A verify command that reads other history is judged on a commit whose history differs from the landed commit's. Examples are commit messages, `git log` and authorship. Such a repository should keep depth 1.
  - **Candidate-branch writers.** Repository writers outside Fishhawk can move candidate branches. That is detected, not prevented.
- **Implementation.** Likely slices:
  1. the `merge_queue` spec key and D1's clamps;
  2. candidate rows, building and invalidation;
  3. the runner capability and gate-environment injection;
  4. landing, with the reservation and the pre- and post-checks;
  5. read surface, wait metric and audit categories;
  6. restart reconciliation and candidate-branch sweeping.

  The spec key follows the `spec-change` checklist, and the wire fields follow the cross-module wire-type rules.

### Validation

- **Order.** Drive a three-entry train at depth 3:
  - entries land in order;
  - entry 2 lands with no push to its branch and no second pass;
  - each landed pair equals its candidate's pair.
- **Red at position 2.** It is held until position 1 passes. Then:
  - entry 2 is ejected;
  - entry 3 is rebuilt on C₁ under a new identity and re-verified;
  - a late result for entry 3's old candidate changes nothing.
- **Red at position 3 before position 2 has a verdict.** It is held. When position 2 then goes red, entry 3's red is discarded.
- **Stale test merge.** Right after a landing, the next position holds and lands when the test merge is current. It does not rebuild.
- **External merge.** Merge to the base outside the queue between check (b) and the merge. Then:
  - the post-check on L fires `merge_queue_landed_tree_mismatch`;
  - the base stops at depth 1;
  - a later matching tip does not clear the stop.
- **Runner without the capability.** It yields depth 1. A runner that drops `diff_base_sha` has its result refused. Cover both the local and `github_actions` runners.
- **Conflict with an entry ahead.** It holds the entry without ejecting it, and the entry rejoins after the conflicting entry settles.
- **Promotion.** Promoting C in [A, B, C] invalidates from position 2. A keeps its candidate and pass, and B is rebuilt on C's new candidate.
- **No verify command.** At declared depth 3, an otherwise eligible entry without a verify command completes through the depth-1 cycle. It is not skipped or stranded, and it is never given a passed candidate verdict.
- **Reservations.**
  - A forge refusal at landing releases the reservation only after the PR reads unmerged.
  - An unreadable L pages the operator and blocks the base.
  - A refusal after the PR head moved releases the reservation and invalidates the position; the base is not blocked.
  - A stale test merge waits with no claim held. A failed re-read under the lock releases the claim and dispatches nothing.
- **Depth clamps.** Each D1 clamp, and a fork head.
- **Restart.** Restart during an unresolved landing reconciles with the forge and never redispatches. A moved candidate branch is invalidated.
- **Detection.** Independent mutations must reach named behaviour assertions:
  - dropping the post-check;
  - dropping the identity check;
  - using B instead of Cₖ₋₁ as the diff base;
  - acting on a red before the positions ahead pass.

  Compilation errors, skipped tests and timeouts are not detection.

- **Record metadata.** amends: ["ADR-090", "ADR-092", "ADR-093", "ADR-109", "ADR-112", "ADR-115", "ADR-142"]; supersedes: []; applies_to: []

**Related:** ADR-017, ADR-035, ADR-063, ADR-094, ADR-096, ADR-098.
