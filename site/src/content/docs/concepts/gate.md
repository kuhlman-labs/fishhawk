---
title: gate
description: A condition that must be satisfied before a stage advances — usually a human approval.
---

A gate is a condition attached to a stage that must be satisfied before the run
advances past it. The common case is an approval gate: a person reads what the
stage produced and says yes or no.

```yaml
gates:
  - type: approval
    approvals:
      count: 1
      not: [author, agent]
```

`count` is how many approvals are needed. `not` is the interesting half: it
excludes principals from satisfying the gate. `author` excludes whoever opened
the change; `agent` excludes every non-human principal. A gate that an agent can
satisfy is not a gate.

## Waiting is the default

A run sitting at a gate is not stuck — it is doing its job. Fishhawk does not
time out an approval gate and proceed. The run waits until a person decides, or
until someone cancels it.

## Escalation

A workflow can raise a gate's requirements for changes that touch sensitive
paths: more approvals, a specific team, a higher permission level. The
escalation is declared next to the workflow and fires on the real change, so a
change that reaches into an escalated path cannot be approved under the ordinary
bar.

## Related

- [constraint](/fishhawk/concepts/constraint/) — the mechanical counterpart, which
  asks no one.
- [Approvals](/fishhawk/operating/approvals/) — how to grant one in practice.
