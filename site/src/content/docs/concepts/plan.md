---
title: plan
description: The structured proposal an agent writes before it changes anything.
---

A plan is what an agent produces before it touches code. It is a structured
artifact, not prose: it validates against a schema, it is stored in the audit
log, and it is the thing a human approves.

A plan carries:

- **A summary** — what the change does and why.
- **A file scope** — every file the agent intends to create or modify. This
  becomes a [constraint](/fishhawk/concepts/constraint/) on the implement stage.
- **An approach** — the ordered steps, in enough detail to be argued with.
- **A verification strategy** — how the result will be shown to work, including
  the tests that will exist afterwards.
- **Risks and assumptions** — what the agent is unsure about, and what would
  falsify each assumption.
- **A rollback plan.**

## Approving, rejecting, and amending

Approving a plan admits it as the binding instruction for the implement stage.
An approval can carry conditions, and those conditions reach the implementing
agent as binding text that wins over the plan on conflict.

Rejecting a plan with a reason opens a fresh planning pass, and the reason
propagates to it — so a rejection is a steering instruction, not just a refusal.

## Why plan first

Reviewing a plan is cheap and reviewing a finished pull request is not. A wrong
approach caught at the plan gate costs one stage; caught at the review gate it
costs the implementation as well. The plan gate is also the only point where the
scope is negotiable before it hardens into a constraint.

## Related

- [Plan schema reference](/fishhawk/reference/plan-schema/) — the `standard_v1`
  artifact.
