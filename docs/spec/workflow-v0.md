# Workflow spec v0

Reference for `.fishhawk/workflows.yaml`. The canonical schema is [`workflow-v0.schema.json`](workflow-v0.schema.json) (JSON Schema Draft 2020-12). Examples live in [`examples/`](examples/).

> **Frozen at Day 21** of the v0 build (MVP_SPEC.md §8). Old workflow runs in the audit log remain readable forever; never break this schema in place — bump to a new spec version (`workflow-v1`, `workflow-v2`...) instead.

### Schema evolution policy

`workflow-v0.x` is **additive-only**. New fields are optional; the existing required-field set is frozen. Validators must tolerate unknown fields (JSON Schema Draft 2020-12 §10.3).

**Breaking additions** (required new fields, renamed fields, removed fields, changed semantics of existing fields) require a version bump to `workflow-v1`. During the deprecation window, the backend accepts both `v0` and `v1` workflow specs simultaneously — the version string in the YAML routes each spec to its respective schema and execution path.

**`x-intended-required`** may annotate optional fields that are candidates for required promotion in `workflow-v1`. Annotation semantics and soak-period rules match those in `plan-standard-v1.md`.

## Top-level shape

```yaml
version: "0.3" # required, exactly "0.3" in v0
roles: # optional; named groups referenced by gates
  <role_id>:
    members: ["@org/team", "@user"]
workflows: # required; at least one workflow
  <workflow_id>:
    description: "..."
    on_ci_failure: # optional; auto-retry policy (#276)
      max_retries: 1 # default when the block is absent
    stages: [...]
```

Identifiers (`<role_id>`, `<workflow_id>`, stage `id`s) are `snake_case` — `^[a-z][a-z0-9_]*$`. Member refs are GitHub conventions: `@user` for a single user, `@org/team` for a team. Resolution happens at run time against the GitHub App installation.

## Stages

```yaml
- id: plan
  type: plan | implement | review # closed set; no custom types
  executor: # exactly one of agent or human
    agent: claude-code # any string; v0 ships claude-code
    timeout: 10m       # optional; stage-level override for agent timeout
    verify:            # optional; in-band test gate (see ### Verify gate)
      command: 'scripts/test'
      timeout: '10m'
    # OR
    human: true
  inputs: [<input>...] # optional
  produces: [<artifact>...] # optional
  constraints: [<constraint>...] # optional, only meaningful for implement
  budget: <budget> # optional
  gates: [<gate>...] # optional
```

Stage `id` is unique within the workflow. The `from_stage` field on inputs cross-references it; the validator (E1.3 / #18) enforces this beyond the schema.

### Verify gate

An optional in-band test gate that fires after the agent exits cleanly and before the bundle is committed. When `verify` is absent, the gate is skipped.

```yaml
executor:
  agent: claude-code
  verify:
    command: 'scripts/test'   # executed via sh -c; non-zero exit → category-A
    timeout: '10m'            # optional; defaults to 10m when absent
```

The runner captures combined stdout+stderr into a `verify_run` bundle event so operators can inspect why the gate failed without re-running. Fields: `command`, `exit_code`, `output`, `outcome` (`"passed"` | `"failed"`).

The gate fires only when the agent itself succeeded (i.e., `res.OK` is true) — a failing agent already produces a category-A trace, and re-running the tests against a broken working tree would be misleading.

The full wiring path from spec to runner is deferred: v0 ships `verify` as a documented spec field for authoring surface. The runner reads the gate command from the `--verify-cmd` CLI flag; a follow-up issue will wire the backend to read `executor.verify.command` from the cached spec and deliver it to the runner via the prompt-fetch response.

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
  schema: standard_v1 # plan only; identifies the artifact schema version
  persistence:
    - target: originating_issue | fishhawk_audit_log
      mode: rendered_comment | canonical
      update_on_change: true # republish if the artifact is regenerated
```

`canonical` is the authoritative copy (stored in the audit log). `rendered_comment` is the human-readable echo on the originating tracker (the GitHub issue), kept in sync.

### `originating_issue` + `rendered_comment` — plan-review surface (ADR-020 / #321)

When a plan stage's `produces` declares `target: originating_issue, mode: rendered_comment`, the backend posts the full standard_v1 plan as a markdown document on the triggering issue (E17.2 / #337). This is the **canonical plan-review surface**: reviewers read and approve from the issue thread, not the SPA. The SPA's plan-document page is a read-only mirror with a `View on GitHub` affordance (E17.5 / #340).

`update_on_change: true` wires re-uploads to edit the existing comment in place via `PATCH /repos/{owner}/{repo}/issues/comments/{id}` (E20.1 / #327 surface). The plan's `github_comment_id` lives in the audit log (`KindPlanFull` / `KindPlanUpdated` rows) so a re-upload after the comment was operator-deleted falls back to a fresh `CreateIssueComment`.

When the flag is omitted, the post is one-shot — the comment lands on the first plan upload and re-uploads are silently skipped (the existing post stays as the record).

When the spec doesn't declare `target: originating_issue` at all, the backend falls back to the legacy summary-post path (`#234`): a short summary comment with a link to the SPA's plan-document page.

## Constraints (implement stages)

Exactly one kind per constraint object — combine multiple kinds in the array. Closed set per MVP_SPEC §4.1.

```yaml
constraints:
  - max_files_changed: 30
  - forbidden_paths:
      - "infra/**"
      - ".github/workflows/**"
  - allowed_paths: # mutually informative with forbidden
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
    any_of: [tech_lead, senior_engineer] # or all_of
  sla: 4_business_hours # optional; D-category timeout

# Check gate — placeholder for workflows that delegate to GitHub branch
# protection. Carries no spec-level fields in 0.2 (#254 / ADR-017).
# When a review stage carries a check-only gate, Fishhawk's
# orchestrator queues `gh pr merge --auto --squash` against the
# implement stage's PR (#255) and transitions the review stage to
# `succeeded` immediately. GitHub's auto-merge machinery handles the
# actual merge once the required checks (from branch protection)
# pass — Fishhawk's role is "queue and step out of the way".
- type: check
```

**Where `approval` gates enforce** (ADR-018 / #311):

- **Plan stages**: enforced by Fishhawk. The gate reads `approvers` and `sla` and accepts a decision from any of the convergent surfaces below (ADR-020 / #321). The vote approves intent before any code is written; GitHub has no equivalent.
- **Review stages**: `approvers` is **informational** in v0. Branch protection's required-reviewers is the actual gate; Fishhawk records reviewer activity from `pull_request_review.submitted` events and transitions the review stage to `succeeded` on `pull_request.closed` with `merged=true` (#312). The in-Fishhawk approval API refuses review-stage submissions with `409 review_stage_managed_by_github` and points the caller at the PR. Teams that want strict approver enforcement configure branch protection's required-reviewers.

**Plan-stage approval surfaces** (ADR-020 / #321 — every action reachable from where developers already work):

| Surface              | How                                                                                                                                     | Surface value          |
| -------------------- | --------------------------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| GitHub reply comment | Type `+1` / `👍` / `:+1:` / `lgtm` as a fresh comment on the issue thread (E17.3 / #338 + E17.4 / #339)                                 | `github_reply_comment` |
| GitHub slash command | Type `/fishhawk approve [reason]` or `/fishhawk reject [reason]` on the issue thread                                                    | `github_comment`       |
| HTTP / SPA / CLI     | `POST /v0/stages/{id}/approvals` (used by the SPA's approval surface and `fishhawk plan approve / reject` — E18.1 / #332, E18.2 / #333) | `api` / `ui` / `cli`   |

All surfaces converge on the same `approvals` table row + an `approval_submitted` audit chain entry. The surface-of-origin is recorded in `approval.surface` (closed enum: `api`, `ui`, `cli`, `github_comment`, `github_reply_comment`) so a post-hoc reviewer can attribute the decision to the right UX affordance. The reply-comment surface skips silently on non-approver reactors and unmatched contexts (a generic "+1" reply on an unrelated issue thread isn't an error); the slash and HTTP/CLI paths reply / surface errors loudly. A future polling worker (E17.3b / #360) will add a `github_reaction` surface for click-only thumbs-up reactions GitHub doesn't deliver via webhook.

`blocking_checks` was removed in v0.2 (ADR-017 / #249). Required CI checks are now derived from GitHub branch protection / rulesets at run-create time and snapshotted onto the run row (#251). The `fishhawk_audit_complete` signal is still computed by Fishhawk (#229) and published as a Check Run on the PR (#231) so branch protection can enforce it.

## Agent timeouts (v0.3 additions, #452)

Two optional fields govern the wall-clock cap on agent invocations. Both accept Go duration strings (e.g. `"30m"`, `"1h"`, `"90s"`).

**Three-level precedence** (highest to lowest):
1. `stage.executor.timeout` — the exception; use when a specific stage SLO differs from the rest.
2. `workflow.policy.max_stage_runtime` — the spirit; expresses the workflow's overall runtime envelope.
3. Backend default (15 minutes) — fallback when neither field is set.

```yaml
version: "0.3"
workflows:
  feature_change:
    policy:
      max_stage_runtime: "30m" # workflow-level default for all agent stages
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
          timeout: "10m" # overrides the 30m policy for this stage only
        produces:
          - artifact: plan
            schema: standard_v1

      - id: implement
        type: implement
        executor:
          agent: claude-code
          # no timeout: inherits the 30m workflow policy
        produces:
          - artifact: pull_request
```

`spec.ResolveStageTimeout` on the backend applies this precedence at prompt-fetch time and delivers the resolved value to the runner via `agent_timeout_seconds` in the prompt-fetch response. The runner applies the local 15-minute fallback when the field is 0 (legacy runs with no `workflow_spec` cached on the row, or runs created before this feature landed).

## On CI failure (auto-retry)

```yaml
workflows:
  feature_change:
    on_ci_failure:
      max_retries: 1 # 0 disables; 1 (default) = retry once; max 5
    stages: […]
```

Per-workflow auto-retry policy (#276 / E16). When a required CI check fails on the implement stage's PR, the dispatcher fires a fresh implement workflow_dispatch up to `max_retries` times, threading each retry via `parent_run_id` (#216).

- **`max_retries`** — integer, `0..5`, default `1`. A retry chain of length N means the agent is dispatched `N+1` times total (original + N retries). Set to `0` to disable auto-retry — useful for low-autonomy workflows where a human prefers to re-trigger after inspecting the failure.
- **Trigger predicate**: only the closed set of failing conclusions in `stagecheck.DeriveState` (`failure`, `timed_out`, `cancelled`, `action_required`, `stale`, `startup_failure`) fires a retry. `success` / `neutral` / `skipped` are no-ops.
- **`fishhawk_audit_complete` failures are excluded** from the retry trigger. Retrying won't fix Fishhawk's own audit gaps; that's #229's job.
- **Required-check scoping**: only failures of checks in the run's branch-protection snapshot (#251) count. A failing third-party non-required check doesn't trigger retries.

## Identifier namespaces

| Field                       | Pattern / values                                                             | Notes                                                                                               |
| --------------------------- | ---------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `version`                   | `"0.3"`                                                                      | current value; 0.2 added v0.2's `blocking_checks` drop, 0.3 adds `on_ci_failure.max_retries` (#277) |
| Role / workflow / stage IDs | `^[a-z][a-z0-9_]*$`                                                          | snake_case                                                                                          |
| Member refs                 | `^@[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?$`                                    | GitHub user or team                                                                                 |
| Stage `type`                | `plan` \| `implement` \| `review`                                            | closed set                                                                                          |
| Executor                    | `agent: <string>` xor `human: true`                                          | mutually exclusive                                                                                  |
| Input `source`              | `github_issue` \| `pull_request`                                             | v0; v0.x adds Linear/Jira                                                                           |
| Artifact                    | `plan` \| `pull_request`                                                     | closed set                                                                                          |
| Persistence target          | `originating_issue` \| `fishhawk_audit_log`                                  | closed set                                                                                          |
| Persistence mode            | `rendered_comment` \| `canonical`                                            | closed set                                                                                          |
| Constraint kind             | `max_files_changed`, `forbidden_paths`, `allowed_paths`, `required_outcomes` | exactly one per constraint                                                                          |
| `required_outcomes` items   | `tests_added_or_updated`, `ci_green`                                         | closed set                                                                                          |
| Budget enforcement          | `advisory` \| `blocking`                                                     | v0 ships advisory only                                                                              |
| Gate `type`                 | `approval` \| `check`                                                        | closed set                                                                                          |
| Approvers shape             | `any_of: [<role_id>...]` xor `all_of: [<role_id>...]`                        | one shape per gate                                                                                  |

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
