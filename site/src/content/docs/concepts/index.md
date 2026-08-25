---
title: Concepts
description: The six terms Fishhawk uses precisely — workflow, stage, gate, constraint, plan, audit log.
---

Fishhawk uses six words in a specific way. They are ordinary lowercase nouns,
not product features, and getting them straight makes everything else readable.

| Term | One line |
|---|---|
| [workflow](/fishhawk/concepts/workflow/) | The ordered sequence of stages a change moves through, declared in `.fishhawk/workflows.yaml`. |
| [stage](/fishhawk/concepts/stage/) | One step of a workflow: who executes it, what it consumes, what it produces. |
| [gate](/fishhawk/concepts/gate/) | A condition that must be satisfied before a stage advances. Usually a human approval. |
| [constraint](/fishhawk/concepts/constraint/) | A rule checked against what a stage actually did, not against what it promised. |
| [plan](/fishhawk/concepts/plan/) | The structured proposal an agent writes before it changes anything. |
| [audit log](/fishhawk/concepts/audit-log/) | The append-only, hash-chained record of every decision and outcome. |

## How they fit together

A **workflow** is a list of **stages**. A stage may carry **gates** (checked
before or after it runs) and **constraints** (checked against its output). The
first stage of most workflows produces a **plan**; a later stage implements it.
Every transition, decision, and verdict along the way is written to the **audit
log**.

The distinction that matters most is gate versus constraint. A gate asks a
person a question and waits for the answer. A constraint asks the diff a
question and needs no one. Both can stop a run; only one of them can be
persuaded.
