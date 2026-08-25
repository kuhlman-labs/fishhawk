---
title: workflow
description: The ordered sequence of stages a change moves through, declared in the repository it governs.
---

A workflow is a named, ordered list of stages. It is declared in
`.fishhawk/workflows.yaml` in the repository it governs, which means the policy
is versioned, diffed, and reviewed exactly like the code it constrains.

```yaml
version: "2"

workflows:
  feature_change:
    description: Default workflow for feature work.
    stages:
      - id: plan
        ...
      - id: implement
        ...
      - id: review
        ...
```

A repository may declare several workflows — one for feature work, one for
dependency bumps, one for documentation — and Fishhawk resolves which applies to
a given change. Resolution can be explicit (the run names the workflow) or
declared (`applies_to` routes by path, label, or trigger).

## Why it lives in the repository

The alternative is a workflow configured in a control plane somewhere else. That
version is easier to change quietly. Keeping the declaration in the repository
means widening an agent's permissions is a diff that a human reviews, and this
repository takes that a step further: `.fishhawk/**` is itself a forbidden path
for the implement stage, so an agent cannot edit the document that constrains it
inside the very change it is proposing.

## Related

- [stage](/fishhawk/concepts/stage/) — what a workflow is made of.
- [Workflow spec reference](/fishhawk/reference/workflow-spec/) — every field,
  and which spec majors are live.
