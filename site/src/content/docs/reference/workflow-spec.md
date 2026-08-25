---
title: Workflow spec
description: .fishhawk/workflows.yaml — the live major, and what an author of a frozen-major document needs to know.
---

`.fishhawk/workflows.yaml` declares the workflows for the repository it sits in.
This page covers all three live majors on purpose: `workflow-v0`, `workflow-v1`
and `workflow-v2` are simultaneously supported off a single `main`, so a
per-major page set would be three pages describing one product. The [support
table](/fishhawk/reference/#workflow-spec-major-support) is the status of each;
[versioning](/fishhawk/reference/versioning/) records why it is laid out this
way.

**Write new specs at `workflow-v2`.** The rest of this page describes v2 unless
it says otherwise; the [frozen majors](#the-frozen-majors-v0-and-v1) section
below is what a `0.x` or `1.x` author needs.

## The shape of a document

```yaml
version: "2"            # required — the single token "2"

defaults:               # optional — file-level executor / reviewers / budget
  executor: {...}

workflows:              # required — at least one
  feature_change:
    description: Default workflow for feature work.
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
```

`version` is required and decides which schema validates the document. At v2 it
is the bare token `"2"`: **v2 has no minor chain.** v0 carried an additive
`0.3`–`0.7` chain and v1 a `1.0`–`1.6` chain, and both gated field acceptance on
the declared minor. At v2 a field is accepted because the schema declares it.

## What a stage declares

| Key | What |
|---|---|
| `id`, `type` | The stage's name and kind (`plan`, `implement`, `review`, …). |
| `executor` | `agent:` for an agent stage, `human: true` for a human one. `verify:` names the test command run before the pull request opens. |
| `inputs` | A `github_issue`, or an `artifact` `from_stage:` an earlier stage. |
| `produces` | The artifact this stage must emit — `plan`, `pull_request`, … |
| `constraints` | Rules checked against the real diff. See [constraint](/fishhawk/concepts/constraint/). |
| `gates` | Approvals that must be satisfied before the run advances. |
| `reviewers` | Advisory or gating review, by agents and/or humans. |
| `budget` | Token and runtime limits, advisory or enforced. |

## v2-only grammar

Three things exist only at v2, and are the reason to prefer it:

- **Same-document reuse.** File-level and workflow-level `defaults`, plus
  workflow `extends`, resolved before schema validation — so a stage can inherit
  its executor rather than repeating it.
- **Shape normalization.** `constraints` as an object, `auto_advance`, and the
  `needs:` shorthand, rewritten at parse time into the older representation.
  Nothing downstream changed; the document just reads better.
- **Autonomy grammar.** An `autonomy:` tier shorthand plus a per-action-class
  matrix (`report` / `gated` / `auto`), which replaced v1's `operator_agent`
  block.

v2 also *removes* several v0/v1 back-compat forms. Each removed or renamed form
is rejected with a message naming its replacement, so a v1 document pasted into
a v2 file fails loudly rather than silently meaning something else.

## The frozen majors: v0 and v1

If you maintain a document that declares `version: "0.x"` or `"1.x"`, this is
what applies to you:

- **It keeps working, with no action.** Both schemas stay embedded, compiled,
  routed and validated. That is pinned by a test, not by a convention.
- **Nothing rejects a new `0.x` or `1.x` document either.** It is accepted; it
  is discouraged, because the reuse, shape and autonomy grammar above exists
  only at v2 and `fishhawk init` emits v2 presets.
- **No removal date is set, and none is planned.**
- **To move, run `fishhawk migrate-spec`.** It translates an existing v1
  document and prints an approval-eligibility report rather than rewriting
  blind. The field-by-field migration path is in
  [`docs/spec/workflow-migration.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/spec/workflow-migration.md).

A document declaring a major above 2 fails closed — it is not routed to the
newest schema on the assumption that newer is compatible.

## Validating

```sh
fishhawk validate ./.fishhawk/workflows.yaml
```

Validate with the CLI or the backend rather than with a bare JSON Schema
runner: a v2 document using `defaults` or `extends` needs same-document reuse
resolved before the schema sees it, and a bare validator rejects a spec the
product accepts.

## The canonical reference

Every field of every major is documented in
[`docs/spec/`](https://github.com/kuhlman-labs/fishhawk/tree/main/docs/spec),
alongside the JSON Schemas themselves. That is the source of truth; this page is
the orientation.
