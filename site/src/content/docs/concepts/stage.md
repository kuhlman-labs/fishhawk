---
title: stage
description: One step of a workflow — who executes it, what it consumes, what it produces, what it may not do.
---

A stage is one step of a workflow. It declares four things:

- **Who executes it.** An agent, a human, or neither. An agent stage spawns the
  runner; a human stage waits.
- **What it consumes.** An issue, or an artifact produced by an earlier stage.
- **What it produces.** An artifact — a plan, a pull request, a report.
- **What it may not do.** Constraints, checked against the result.

```yaml
- id: implement
  type: implement
  executor:
    agent: claude-code
  inputs:
    - artifact: plan
      from_stage: plan
  produces:
    - artifact: pull_request
  constraints:
    - forbidden_paths: [".github/workflows/**"]
```

## Stage state

A stage is `pending`, then `dispatched`, then `running`, then terminal —
`succeeded`, `failed`, or `cancelled`. A stage that produces an artifact does
not reach `succeeded` until the artifact exists and its constraints pass.

Stages are ordered, and by default sequential: the next one does not start until
the previous one is terminal and its gates are satisfied. A workflow may declare
dependencies explicitly when the order is not a straight line.

## Related

- [gate](/fishhawk/concepts/gate/) — what holds a stage before or after it runs.
- [constraint](/fishhawk/concepts/constraint/) — what is checked against what it
  produced.
