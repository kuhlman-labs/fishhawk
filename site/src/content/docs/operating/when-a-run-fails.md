---
title: When a run fails
description: Triage a stopped run into parked, failed, or stranded; read the failure category; match a signature; and choose retry, revive, fix-up, reap, or abandon.
---

This is the page you reach for when a run stopped and it is not simply waiting
for you. Triage first, then category, then the specific signature, then the verb.

## Triage first: parked, failed, or stranded

Three states stop a run, and they call for different actions:

- **Parked** — waiting for a decision from you. A run at a
  [gate](/fishhawk/concepts/gate/) is doing its job. This is not a failure;
  go to [deciding at a gate](/fishhawk/operating/approvals/).
- **Failed** — a stage hit a terminal condition with a recorded **failure
  category**, and the whole run flipped `failed`. The rest of this page is about
  this state.
- **Stranded** — the state readers do not expect: a stage sitting in
  `dispatched` or `running` with **no live runner** behind it. The tell is that
  it never advances while status keeps reporting it in-flight, and that
  **retry, revive, fix-up and dispatch all refuse it**. Its only exit is the
  reap-failure endpoint followed by a retry (below).

Answer "which of the three" before touching a recovery verb.

## The four categories decide what is even possible

Every failed stage carries a category, and the category — not the symptom —
decides your options:

| Category | Meaning | Retryable? |
|---|---|---|
| **A** | Agent — a timeout, a transport error, a model-side failure | **Yes**, retry in place |
| **B** | Constraint or policy — the change broke a declared boundary | **No** — amend scope or replan |
| **C** | Infrastructure — a flake, a lock, a runner that died | **Yes**, retry in place |
| **D** | Approval timeout | Re-open at the gate |

The load-bearing line is **B is not retried.** A category-B failure means the
change exceeded its declared scope, wrote a forbidden path, or missed a required
outcome — the system working, not breaking. Retrying it unchanged fails it
again; the fix is to amend the scope or replan, and re-opening a B stage at all
needs an explicit operator override. A and C are the retryable classes.

## The failure catalogue

When a stage fails, the product matches its evidence — the category, the failure
reason, the runner's progress counters — against a shipped registry of named
**failure signatures**, and on a match `next_actions` carries a `signature` block
naming what the failure means and an ordered recovery. The registry is
[`docs/architecture/failure-signatures.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/architecture/failure-signatures.md)
(source of truth
[`backend/internal/failuresig/registry.go`](https://github.com/kuhlman-labs/fishhawk/blob/main/backend/internal/failuresig/registry.go)).

Read one property before you rely on it: the block is **display-only and
fail-open**. When nothing matches, the block is simply **absent** and the rest of
the output is unchanged — so its absence means "no signature matched," never
"nothing is wrong." The matchers key on substrings of what the runner emits, so a
wording change on the emitting side yields a *missing* hint, never a wrong one.

The eight shipped signatures, in the reader's terms:

- **`external_api_incident`** — the model provider returned a terminal error
  after exhausting retries (often a 529 overload). This is an upstream incident,
  not a defect in the task. **Back off** until the provider's status page clears
  it, *then* retry — an immediate retry re-hits the same incident and burns retry
  budget. Checked first: it wins over an absorbed infra flake, because the
  recoveries are opposites.
- **`model_quota_exhausted`** — the agent could not obtain model quota; a usage
  or rate cap, not a crash (the attempt made no model call). It will fail
  identically until the window resets. Wait for the reset, then retry.
- **`slice_integration_conflict`** — a decomposed parent's fan-in could not merge
  one slice branch onto the consolidated branch. The consolidated branch already
  holds the earlier slices, so the **parent is not the thing to re-drive**.
  Resume the *conflicting child's* run (read its run id from the newest
  `slice_integration_conflict` audit entry's structured payload, not the reason
  string); resuming the parent replans from scratch and discards the succeeded
  siblings.
- **`lineage_lock_contention`** — the runner refused to start because another
  runner already holds this run's lineage lock: two runners were pointed at the
  same lineage, or a previous runner's lock outlived it. Confirm no live runner
  still holds it (`pgrep -f fishhawk-runner`); dispatch decomposition children
  **sequentially**, not concurrently, which is the usual cause; then retry.
- **`zero_exit_strand`** — the runner exited *successfully* having settled
  nothing: it re-entered a phase that had already completed, did no work, and
  left the stage stranded. A retry usually re-runs the same no-op — do not spend
  more than one; if it recurs, cancel and start a fresh run.
- **`runner_died_before_reporting`** — the runner exited non-zero without ever
  reporting a terminal state, so the backend reaped the stage. The real cause is
  in the runner log, not the stage's failure reason — read the dispatch's log,
  check the host for a crash it could not report (out of memory, a killed
  process), then retry to re-spawn.
- **`infra_flake_recurred`** — the verify gate hit an infrastructure flake,
  absorbed one in-place re-run, and the flake recurred: the environment, not the
  change. A recurring absorbed flake is the cheapest thing to retry; if it recurs
  again, check the local Docker/testcontainers state before spending a third.
- **`agent_no_progress_repeat`** — a repeat attempt failed category-A while its
  heartbeat reported **zero turns and zero tokens**: the agent never got going,
  so this is a harness or provider stall, not a hard task. Check the provider
  status page and read the trace for a first-turn harness error before spending
  another retry; if a third attempt also reports zero turns, stop and read the
  runner log.

## The recovery verbs

- **Retry** re-opens **one** stage and dispatches it. It admits only a failed,
  retryable (A/C) stage. `fishhawk run retry <stage-id>` on the CLI, or the
  `fishhawk_retry_stage` MCP tool.
- **Revive** re-admits a **whole** terminal-failed run: it re-parks every failed
  stage in its correct pre-dispatch state and **never dispatches**, so you can
  never end up half-recovered or re-dispatched out of gate order. It
  **pre-validates that every failed stage is retryable and refuses the whole
  revive** if any one is not (category-B, D-rejected, or without a recorded
  category), naming the blocking stage — no partial mutation. MCP
  `fishhawk_revive_run`; no CLI verb.
- **Fix-up** re-opens the implement stage for another agent pass against the same
  run — the route for a review concern worth an agent pass. MCP
  `fishhawk_fixup_stage`; no CLI verb.
- **Reap-failure** is the stranded-stage escape: it reports the stuck stage
  failed/category-C, which is exactly the state retry admits. On a local runner
  it refuses if a runner is still alive — wait instead. Then retry. MCP
  `fishhawk_reap_stage`; no CLI verb.

Each re-open consumes that stage's retry budget — revive is a batch retry, not a
budget bypass.

## What is not recoverable

Some states have no path out, and knowing that saves you hunting for a verb that
does not exist:

- **Nothing un-cancels a run.** Cancelling is terminal. Cancelling a
  *decomposition child* wedges its parent **permanently**, because consolidation
  requires every child to have succeeded and a cancelled child never can. Do not
  reach for cancel to clear a stranded child — reap it.
- **The fix-up ceiling is hard at three passes.** Past it the fix-up verb
  refuses. The remedy is *not* a fourth pass: it is an operator-vouched commit on
  the run branch (commit the one-line fix yourself, then vouch it so it clears
  the sole-writer lineage gate), or merging with a follow-up issue, or starting
  fresh.
- **A category-B failure is not made retryable by retrying it.** Amend the scope
  or replan.

And the general rule: **a run whose base has drifted or whose branch state is
ambiguous is cheaper to abandon and re-run than to argue with.** Not every state
has a clean path out. If triage does not land on parked, a matched signature, or
a plainly retryable A/C failure, abandon and re-run rather than iterating against
a confused state.

## Where the evidence is

- The [audit log](/fishhawk/concepts/audit-log/) carries the decision record —
  who did what, when, with what reason.
- The **stage trace** carries what the agent actually did. A stage that reports
  success is not proof the work happened: diff the commit against what was asked
  before trusting a self-report.
- The **gate view** (`fishhawk_get_gate_view`) reads the full concern prose in
  one call when a review is involved.
