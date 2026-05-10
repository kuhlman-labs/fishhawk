# Workflow spec v0

Reference for `.fishhawk/workflows.yaml`. The canonical schema is [`workflow-v0.schema.json`](workflow-v0.schema.json) (JSON Schema Draft 2020-12). Examples live in [`examples/`](examples/).

> **Frozen at Day 21** of the v0 build (MVP_SPEC.md §8). Old workflow runs in the audit log remain readable forever; never break this schema in place — bump to a new spec version (`workflow-v1`, `workflow-v2`...) instead.

## Top-level shape

```yaml
version: "0.2"          # required, exactly "0.2" in v0
roles:                  # optional; named groups referenced by gates
  <role_id>:
    members: ["@org/team", "@user"]
workflows:              # required; at least one workflow
  <workflow_id>:
    description: "..."
    stages: [...]
```

Identifiers (`<role_id>`, `<workflow_id>`, stage `id`s) are `snake_case` — `^[a-z][a-z0-9_]*$`. Member refs are GitHub conventions: `@user` for a single user, `@org/team` for a team. Resolution happens at run time against the GitHub App installation.

## Stages

```yaml
- id: plan
  type: plan | implement | review     # closed set; no custom types
  executor:                           # exactly one of agent or human
    agent: claude-code                # any string; v0 ships claude-code
    # OR
    human: true
  inputs:      [<input>...]           # optional
  produces:    [<artifact>...]        # optional
  constraints: [<constraint>...]      # optional, only meaningful for implement
  budget:      <budget>               # optional
  gates:       [<gate>...]            # optional
```

Stage `id` is unique within the workflow. The `from_stage` field on inputs cross-references it; the validator (E1.3 / #18) enforces this beyond the schema.

## Inputs

Two shapes:

```yaml
# External trigger (issue, PR)
- source: github_issue | pull_request
  required: true

# Artifact from a prior stage in the same run
- artifact: plan | pull_request
  from_stage: <stage_id>
```

## Produces

```yaml
- artifact: plan | pull_request
  schema: standard_v1                 # plan only; identifies the artifact schema version
  persistence:
    - target: originating_issue | fishhawk_audit_log
      mode: rendered_comment | canonical
      update_on_change: true          # republish if the artifact is regenerated
```

`canonical` is the authoritative copy (stored in the audit log). `rendered_comment` is the human-readable echo on the originating tracker (the GitHub issue), kept in sync.

## Constraints (implement stages)

Exactly one kind per constraint object — combine multiple kinds in the array. Closed set per MVP_SPEC §4.1.

```yaml
constraints:
  - max_files_changed: 30
  - forbidden_paths:
      - "infra/**"
      - ".github/workflows/**"
  - allowed_paths:                    # mutually informative with forbidden
      - "docs/**"
      - "**/*.md"
  - required_outcomes:
      - tests_added_or_updated
      - ci_green
```

Constraints are evaluated **post-hoc on the runner** (E5.5 / #53) against the produced diff. Hits become **category B** failures (MVP_SPEC §6).

## Budget

```yaml
budget:
  max_tokens: 200000
  max_runtime_minutes: 15
  enforcement: advisory | blocking
```

v0 ships `advisory` enforcement only — the runner reports overruns but does not abort. `blocking` arrives in v0.x once Fishhawk issues ephemeral agent keys (so the proxy can hard-cap).

## Gates

Two types:

```yaml
# Approval gate — blocks until an approver acts.
- type: approval
  approvers:
    any_of: [tech_lead, senior_engineer]   # or all_of
  sla: 4_business_hours                    # optional; D-category timeout

# Check gate — placeholder for workflows that delegate to GitHub branch
# protection. Carries no spec-level fields in 0.2 (#254 / ADR-017).
- type: check
```

`blocking_checks` was removed in v0.2 (ADR-017 / #249). Required CI checks are now derived from GitHub branch protection / rulesets at run-create time and snapshotted onto the run row (#251). The `fishhawk_audit_complete` signal is still computed by Fishhawk (#229) and published as a Check Run on the PR (#231) so branch protection can enforce it.

## Identifier namespaces

| Field | Pattern / values | Notes |
|---|---|---|
| `version` | `"0.2"` | current value; 0.1 was frozen briefly before ADR-017 dropped `blocking_checks` (#254) |
| Role / workflow / stage IDs | `^[a-z][a-z0-9_]*$` | snake_case |
| Member refs | `^@[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?$` | GitHub user or team |
| Stage `type` | `plan` \| `implement` \| `review` | closed set |
| Executor | `agent: <string>` xor `human: true` | mutually exclusive |
| Input `source` | `github_issue` \| `pull_request` | v0; v0.x adds Linear/Jira |
| Artifact | `plan` \| `pull_request` | closed set |
| Persistence target | `originating_issue` \| `fishhawk_audit_log` | closed set |
| Persistence mode | `rendered_comment` \| `canonical` | closed set |
| Constraint kind | `max_files_changed`, `forbidden_paths`, `allowed_paths`, `required_outcomes` | exactly one per constraint |
| `required_outcomes` items | `tests_added_or_updated`, `ci_green` | closed set |
| Budget enforcement | `advisory` \| `blocking` | v0 ships advisory only |
| Gate `type` | `approval` \| `check` | closed set |
| Approvers shape | `any_of: [<role_id>...]` xor `all_of: [<role_id>...]` | one shape per gate |

## Validation rules beyond the schema

The schema enforces structure. The validator (E1.3 / #18) layers on:

- Every `inputs[].from_stage` references an existing stage `id` within the same workflow.
- Every `approvers.any_of` / `approvers.all_of` entry references a key in the top-level `roles` map.
- Within a workflow, stage `id`s are unique.
- A stage that produces `plan` includes `schema: standard_v1`.

These are graph-shape checks JSON Schema can't express cleanly.

## See also

- `MVP_SPEC.md` §4.1 — the six primitives and what they're for.
- `MVP_SPEC.md` §4.2 — canonical example (mirrored under [`examples/workflow-v0-feature-change.yaml`](examples/workflow-v0-feature-change.yaml)).
- `plan-standard-v1.md` — the plan artifact schema produced by `type: plan` stages.
