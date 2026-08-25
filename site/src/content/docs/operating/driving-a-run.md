---
title: Driving a run
description: Orientation for the operator loop — stub, filled by E12.3 (#2263).
---

:::note[This page is a stub]
The operator loop — opening a run, dispatching each stage, reading status, and
carrying a change to merge — is written by [E12.3
(#2263)](https://github.com/kuhlman-labs/fishhawk/issues/2263). What follows is
orientation.
:::

## The shape of the loop

You are the operator. The agent proposes; you decide at each gate. A run
advances through mechanical transitions on its own and stops at every gate.

1. **Open a run** against an issue.
2. **Run the plan stage.** It settles when the agent has written a plan and the
   advisory reviewers have returned verdicts.
3. **Read the plan and its reviews, then decide.** Approve — optionally with
   binding conditions — or reject with a reason that steers the replan.
4. **Dispatch the implement stage.** It opens a pull request.
5. **Wait for the implement review** to reach a terminal verdict.
6. **Merge**, once the review and any acceptance stage are satisfied.

## Three interfaces, one loop

The same loop is drivable from the CLI (`fishhawk`), the Web UI, and the MCP
server. The MCP server is what lets an agent in your editor drive the loop
conversationally; the CLI is what you script. They are views over the same run
state, not three separate products.

## What to watch for

Stages that need a decision from you surface as such — a run parked at a gate is
waiting, not broken. A run that has genuinely failed is a different state, and
[when a run fails](/fishhawk/operating/when-a-run-fails/) covers the recovery
verbs.
