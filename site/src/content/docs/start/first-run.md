---
title: Your first run
description: Orientation for the first end-to-end run — stub, filled by E12.2 (#2262).
---

:::note[This page is a stub]
The end-to-end walkthrough — installing the binaries, pointing Fishhawk at a
repository, opening a run against an issue, and approving the plan — is written
by [E12.2 (#2262)](https://github.com/kuhlman-labs/fishhawk/issues/2262). What
follows is orientation so you know what the walkthrough will cover.
:::

## What a first run involves

A run is opened against an issue in your tracker. Fishhawk reads
`.fishhawk/workflows.yaml` from the repository, resolves which workflow applies,
and creates the run's first stage.

The shape you will walk through:

1. **Bring up the stack.** Postgres and the control plane, locally via `make up`
   and `make dev-backend`. The [repository
   README](https://github.com/kuhlman-labs/fishhawk#quickstart) carries the
   current quickstart commands.
2. **Declare a workflow.** `fishhawk init` scaffolds `.fishhawk/workflows.yaml`
   from an autonomy preset. Validate it with `fishhawk validate`.
3. **Open a run** against an issue. The plan stage dispatches an agent, which
   writes a plan: a scope, an approach, a verification strategy.
4. **Read the plan and decide.** Approve it and the implement stage runs;
   reject it with a reason and the agent replans against your feedback.
5. **Review the pull request.** The implement stage opens one. The review gate
   is a human approval — and by default it cannot be the change's author.

## Before you start

You need Go, Docker, and an agent CLI on `PATH`. The Web UI additionally needs
Node and pnpm. Nothing about the first run requires a hosted deployment; the
whole loop works against a local backend.

Once you have run it once, [Concepts](/fishhawk/concepts/) explains why each
step exists.
