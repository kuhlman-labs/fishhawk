---
title: Driving a run
description: The operator loop as a state machine — the transitions a run makes on its own, the judgment points where it stops for you, and the verb for each.
---

You are the operator. The agent proposes; you decide at each gate. This page is
the loop that ties the other four Operating pages together: how a run moves, how
to tell the state it is in, and which verb advances it.

## The loop is a state machine, and it stops for you on purpose

A run does not need you for every step. The shipped `feature_change` workflow
declares `auto_advance: true`, so `fishhawkd` advances the *mechanical*
transitions itself — a review settling, a fix-up re-parking the implement stage,
the derived `awaiting_merge` presentation — and stops at the *judgment* points,
where a person has to decide.

That distinction is the whole loop. It means the first question at any moment is
always **which state am I in**, and the two you most need to tell apart are:

- **Parked.** The run reached a gate and is waiting for a decision from you. A
  run at a gate is doing its job, not stuck — Fishhawk does not time out an
  approval gate and proceed. See [gate](/fishhawk/concepts/gate/).
- **Failed.** A stage hit a terminal condition with a recorded failure category,
  and the whole run flipped `failed`. This needs recovery, not a decision.

A third state, **stranded**, is rarer and covered on
[when a run fails](/fishhawk/operating/when-a-run-fails/): a stage sitting in
`running` with no live runner behind it. Reach for that page the moment a run is
not simply waiting for you.

## The shape, gate by gate

1. **Open a run** against an issue.
2. **Run the plan stage.** It settles when the agent has written a
   [plan](/fishhawk/concepts/plan/) and the advisory reviewers have returned
   verdicts.
3. **Decide at the plan gate.** Approve — optionally with binding conditions —
   or reject with a reason that steers the replan. This is the cheapest place to
   correct course; [deciding at a gate](/fishhawk/operating/approvals/) covers
   what to read and how conditions bind.
4. **Dispatch the implement stage.** The agent executes the approved plan and
   opens a pull request.
5. **Wait for the implement review.** Two advisory reviewers read the diff; see
   [advisory reviews and disagreement](/fishhawk/operating/reviews/).
6. **Decide at the review gate, then the merge gate.** Approve the pull request
   under your own identity and merge, once the review — and any acceptance
   stage — is satisfied.

## One loop, three interfaces, and where the verbs are not symmetric

The same run state is drivable from three surfaces: the `fishhawk` CLI, the web
UI, and the MCP server (which is what lets an agent in your editor drive the loop
conversationally). They are views over one run, not three products.

They are **not** symmetric in what they can do, and knowing the asymmetry saves
you looking for a CLI verb that does not exist. The CLI is the spine for the
happy path — it dispatches exactly:

- `fishhawk run` — `start`, `status`, `list`, `cancel`, `open`, `retry`,
  `watch`.
- `fishhawk plan` — `approve`, `reject`, `revise`.

Everything else an operator does at a gate — routing a review concern to a
fix-up pass, waiving or deferring a concern, deciding a scope amendment, reaping
a stranded stage, reviving a failed run, vouching an operator commit, arbitrating
acceptance, and the merge verb — is a REST endpoint
([`docs/api/v0.openapi.yaml`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/api/v0.openapi.yaml))
and an MCP tool
([`backend/internal/mcpserver/`](https://github.com/kuhlman-labs/fishhawk/tree/main/backend/internal/mcpserver)),
with **no CLI verb today**. When a recovery or gate page names one of those, it
names the MCP tool because that is the surface that has it.

Two consequences of the CLI note: `fishhawk run retry` takes a **stage** id, not
a run id — retry is stage-scoped, and only a failed, retryable stage is
admitted. And `fishhawk run cancel` is terminal: nothing un-cancels a run.

## `next_actions` is the authority; this page is not

Every place this guide writes a sequence of steps, it is describing the model.
The **authoritative** statement of what to do next is `next_actions`: a
run-state-aware block the server computes from the run's actual state — its
classified lifecycle state, the legal verbs from there, and, on a failed run, a
matching [failure signature](/fishhawk/operating/when-a-run-fails/) naming the
recovery. Because it is derived from live state rather than from your memory of
the procedure, **when `next_actions` and any sequence written here disagree,
`next_actions` wins.**

Read the honest limit of where it appears. `next_actions` is produced by
[`backend/internal/mcpserver/next_actions.go`](https://github.com/kuhlman-labs/fishhawk/blob/main/backend/internal/mcpserver/next_actions.go)
and rides the **MCP tool responses** — `fishhawk_get_run_status` and the
run-terminal `fishhawk_run_stage` result. It is **not** on `GET /v0/runs/{id}`
and the web UI does not render it. If you drive from the CLI or the UI, you do
not see this block; what you read instead is the stage state (`fishhawk run
status <run-id>`) plus the decision record in `fishhawk audit list`. The REST run
GET does carry a thinner, separate drive `next_action` and a `delegation.matrix`,
but neither is the recovery block the MCP tools render.

## The loop is not interactive — plan for the wait

A plan stage runs for minutes; an implement stage runs for tens of minutes. The
loop is asynchronous, so you have two honest options rather than a live prompt:

- **Block on it.** The MCP `fishhawk_await_stage` / `fishhawk_await_review` tools
  hold until the stage or review reaches a terminal state and then release with
  the next step pre-filled. This is the least busy-work path if you are driving
  from an editor.
- **Poll it.** `fishhawk run watch` polls a run's state from the terminal;
  `fishhawk run status` is a single read. Poll on your own cadence rather than
  spinning.

Either way, budget for a stage that takes real time — do not read a long-running
implement stage as a hang.

## Before you dispatch on a local runner: a clean tree

On the local dogfood runner the working tree you dispatch from is the base the
runner builds on. A failed run can leave that tree dirty — `working_tree_restored`
leaves untracked files behind, and a failed fix-up can leave staged changes. Run
`git status` before every dispatch: an unclean tree lets the next runner sweep
leftovers into the commit and fail the stage on scope drift (a constraint
failure). Recover with `git checkout -f main` or by removing the untracked files
so each run starts clean. The runner owns every commit, branch and push — never
commit or switch branches yourself.

## A normal week

A change lands like this. You open a run against an issue and run the plan stage.
You read the plan and its two advisory reviews, and — because the scope and
approach are right — you approve, folding one narrow correction into the approval
reason. You dispatch implement and go do something else; a while later the review
has settled with one reviewer flagging a concern the other did not. You read both
verbatim, decide the concern is worth an agent pass, and route it to a fix-up.
The next review is clean, so you approve the pull request under your own name and
merge.

When it does not go like that:

- The plan is aimed wrong → [deciding at a gate](/fishhawk/operating/approvals/).
- The reviewers disagree, or one rejects →
  [advisory reviews and disagreement](/fishhawk/operating/reviews/).
- A stage fails, or a run strands →
  [when a run fails](/fishhawk/operating/when-a-run-fails/).
- You want fewer of these decisions to reach you →
  [tuning autonomy](/fishhawk/operating/autonomy/).
