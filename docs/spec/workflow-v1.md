# Workflow spec v1

Reference for `.fishhawk/workflows.yaml` at major version 1. The canonical schema is [`workflow-v1.schema.json`](workflow-v1.schema.json) (JSON Schema Draft 2020-12).

> **v1 began as a structural copy of v0 (ADR-046 / #1381) and now adds the deploy surface (E23.2 / #1382).** The inherited `$defs` and `properties` stay byte-for-byte identical to [`workflow-v0.schema.json`](workflow-v0.schema.json); v1 layers the delegating deploy grammar (per ADR-038 / #925) on top — the `deploy` stage type, the `deployment` artifact, the delegating executor, and three pre-flight constraint kinds. **v0 stays frozen** and rejects `deploy` via its closed enums, so a v0 spec carrying a deploy stage fails at the schema layer.

## Grammar

Every v0 field is inherited unchanged. For the full base reference (top-level shape, stages, executors, inputs, produces, constraints, budgets, gates, operator-agent delegation — including the `operator_agent.model_policy` scenario-A model-selection contract (#1421), inherited verbatim and surfaced identically on the run-status delegation block — decomposition controls), see [`workflow-v0.md`](workflow-v0.md). The v1 additions are the [deploy stage](#deploy-stage-v1) members below. A minimal non-deploy v1 spec differs from a v0 spec only in its `version` value:

```yaml
version: "1.0" # required; routes to workflow-v1.schema.json
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
```

## Deploy stage (v1)

The `deploy` stage type is **delegating-only** (ADR-038 / #925): Fishhawk orchestrates and gates the release but holds **no deploy logic or credentials**. A deploy stage hands execution to an external pipeline and captures the outcome as a `deployment` artifact. The deploy members are bound together by the semantic validator (`backend/internal/spec/validate.go`) because the executor and constraint schema `$def`s are shared across every stage type and so can't express the type-specific pairing themselves:

- A **deploy stage MUST** use a delegating executor (`executor.delegate`) and **MUST NOT** use an `agent` or `human` executor.
- A **non-deploy stage MUST NOT** use `executor.delegate`.
- The **pre-flight constraint kinds** (`allowed_environments`, `change_freeze`, `required_upstream`) are valid **only** on a deploy stage.
- The **post-hoc diff constraint kinds** (`max_files_changed`, `forbidden_paths`, `allowed_paths`, `required_outcomes`) are **not** valid on a deploy stage — a delegating deploy produces no reviewable diff.
- The **`deployment` artifact** is valid **only** on a deploy stage.

### Delegating executor

`executor.delegate` names the external pipeline via a `target` discriminator:

| `target` | Required | Optional | Meaning |
|---|---|---|---|
| `github_actions` | `workflow_ref` | `git_ref` | Dispatch a GitHub Actions workflow via `workflow_dispatch`. `workflow_ref` is the workflow file or id (e.g. `deploy.yml`); `git_ref` is the branch/tag/sha to dispatch against (absent = the provider default). |
| `webhook` | `url` | — | POST the deploy trigger to a generic webhook endpoint. |

### deployment artifact

The `deployment` artifact records the delegated release outcome — its runtime shape is `{environment, ref/sha, external_run_url, outcome, rollback_handle}`. This schema slice only declares the artifact so a deploy spec parses and validates; the runtime that populates it is downstream (the run lifecycle / runner that consume the spec).

### Pre-flight constraints

The three pre-flight deploy constraint kinds are evaluated **before** the stage executes (a pre-execution gate), distinct from the post-hoc diff constraints evaluated against a produced diff:

| Kind | Shape | Meaning |
|---|---|---|
| `allowed_environments` | array of strings (min 1) | The deploy stage may target only these environments. |
| `change_freeze` | boolean | When `true`, the stage is blocked while a change freeze is active. The freeze-signal source is out of scope for the spec (it belongs to the consuming runtime). |
| `required_upstream` | array, unique, items `review_merged` \| `ci_green` (min 1) | Upstream conditions that must hold before the stage may run. |

### Example — a gated deploy stage

```yaml
version: "1.0"
roles:
  release_manager:
    members: ["@kuhlman-labs"]
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions # or: webhook + url
            workflow_ref: deploy.yml
            git_ref: main
        constraints:
          - allowed_environments: [production]
          - change_freeze: true
          - required_upstream: [review_merged, ci_green]
        produces:
          - artifact: deployment
        gates:
          - type: approval # pre-execution operator gate
            approvers:
              any_of: [release_manager]
```

See [ADR-038 (#925)](https://github.com/kuhlman-labs/fishhawk/issues/925) for the delegating-only deploy decision and epic [#924](https://github.com/kuhlman-labs/fishhawk/issues/924) for the deploy workstream.

## Reviewer policy (v1)

The inherited `reviewers.agents[]` heterogeneous reviewer list (#955) gains **additive optional** per-reviewer fields in v1.x: `reasoning_effort` (#1493) and `optional` (#1495). `reasoning_effort` is a **codex-only** knob — the anthropic and claudecode adapters take no reasoning-effort parameter and ignore it; `optional` is the per-reviewer capability-gap degradation policy (see below). The `reviewers` block itself also gains `review_timeout` (#1494).

```yaml
version: "1.0"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agents:
            - provider: codex
              reasoning_effort: high # low | medium | high | xhigh | max
            - provider: anthropic # no reasoning_effort — ignored anyway
          human: 1
```

`reasoning_effort` is resolved through a two-rung ladder, lowest precedence to highest:

```
deployment default (FISHHAWKD_CODEX_REASONING_EFFORT)  <  reviewers.agents[i].reasoning_effort
```

- A **non-empty** spec value wins and is passed to the codex adapter as a `-c model_reasoning_effort=<effort>` CLI override.
- An **empty/absent** spec value falls back to the deployment default exactly as before this field existed; when both are empty the codex CLI inherits the host `~/.codex` config.

The schema `enum` (`low | medium | high | xhigh | max`) is the sole guard before the value reaches the codex CLI — an out-of-enum value is rejected at spec validation. This mirrors the `executor.model` per-stage override (#1013) and the model-resolution ladder (#1416); it moves what was a single deployment-global `FISHHAWKD_CODEX_REASONING_EFFORT` knob into the versioned, per-reviewer spec.

### `reviewers.review_timeout` (#1494)

The `reviewers` block gains a second **additive optional** field in v1.x: `review_timeout`, a duration string (`time.ParseDuration` form, e.g. `5m`, `600s`). It sets the **Floor** rung of the size-aware review-wait budget (`Floor + PerKB*ceil(promptKB)`, clamped to `[Floor, Cap]`) for **this stage's** agent reviews, so plan and implement stages can carry different review timeouts.

```yaml
version: "1.0"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          agent: 1
          human: 0
          review_timeout: 5m # this stage's review-budget floor
      - id: implement
        type: implement
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 0
          review_timeout: 10m # implement diffs are larger — a longer floor
```

`review_timeout` is resolved through a two-rung ladder, lowest precedence to highest:

```
deployment default (FISHHAWKD_PLAN_REVIEW_TIMEOUT)  <  reviewers.review_timeout
```

- A **non-empty**, parseable spec `review_timeout` **overrides** the `FISHHAWKD_PLAN_REVIEW_TIMEOUT` deployment default for that stage's review budget floor.
- An **empty/absent** (or unparseable) value falls back to the `FISHHAWKD_PLAN_REVIEW_TIMEOUT` deployment default exactly as before this field existed.
- Only the **Floor** rung is per-stage; the size-aware `PerKB` and `Cap` rungs (`FISHHAWKD_REVIEW_BUDGET_PER_KB` / `FISHHAWKD_REVIEW_BUDGET_CAP`) stay deployment-level.

The schema `pattern` (`^([0-9]+(ns|us|ms|s|m|h))+$`) is the guard at spec validation; the value is resolved by `spec.ResolveReviewTimeout`, mirroring `spec.ResolveStageTimeout`'s spec-wins precedence.

### `reviewers.agents[i].optional` (#1495)

Each `reviewers.agents[]` entry gains a third **additive optional** field in v1.x: `optional` (boolean, default `false`). It makes the **spec authoritative** for *which* reviewers run and reframes the deployment env flags (`FISHHAWKD_ANTHROPIC_API_KEY` / `FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER` / `FISHHAWKD_ENABLE_CODEX_REVIEWER`) as **capability gates** — "is this provider available on this deployment" — rather than policy switches that silently override the spec.

```yaml
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-4-8
            - provider: codex
              optional: true # unavailable codex degrades QUIETLY; run still proceeds
          human: 0
```

`optional` is the **per-reviewer degradation policy** for the case where a spec-declared reviewer's provider is **unavailable on this deployment** (its capability gate is off):

- `optional: false` (default) — the deployment **should** run this reviewer. An unavailable provider surfaces **loudly** (an `ERROR` log naming the env knob + a capability audit) but **does not block**.
- `optional: true` — a **quiet, graceful advisory-skip** when the provider is unavailable.

Either way run creation **no longer hard-fails** on the capability gap: the spec is valid, only the deployment capability is missing. The gap is recorded as a `reviewer_capability_unavailable` audit at run-create time and, when the review loop runs, as a capability-framed `*_review_skipped` audit (reason `reviewer_unavailable`, carrying `provider` + `optional`) — deliberately **distinct** from a genuine reviewer error (`*_review_failed`), because the reviewer never ran.

**Before / after gating behavior:**

| Deployment state (gating plan stage, `human: 0`) | Before #1495 | After #1495 |
|---|---|---|
| Spec-declared reviewer's provider unavailable, another backend **is** wired | run creation **rejected** (400, `plan_reviewer_unconfigured`) | run **created**; capability audit + `*_review_skipped`, honoring `optional` (loud/quiet); gate not blocked |
| **No** reviewer backend wired at all | run creation rejected (400) | **still rejected** (400) — a deployment-wide misconfiguration, distinct from a per-reviewer gap, `optional` does not apply |

The **coarse** "no reviewer backend wired at all" case remains a hard-fail on **both** run-create paths — the API create-run path (`handleCreateRun`) and the webhook dispatcher (`!PlanReviewerConfigured`) — so they stay symmetric. Only the finer per-reviewer capability gap degrades.

## Version routing

The backend (`backend/internal/spec`) and the CLI (`cli/internal/spec`) compile **both** the workflow-v0 and workflow-v1 schemas at init and dispatch a spec to one of them by its `version` **major** component:

- `version: "0.x"` → `workflow-v0.schema.json`
- `version: "1.0"` → `workflow-v1.schema.json`
- a missing / non-string / unparseable `version` falls through to the v0 schema, which then emits the existing required-version error (so a malformed version never silently passes)
- a well-formed but unrecognized major (`>= 2`) **fails closed** with an error naming the supported majors (`0, 1`)

`/healthz` advertises both the `workflow-v0` and `workflow-v1` embedded-schema hashes so a component can detect drift in either.

## See also

- [`workflow-v0.md`](workflow-v0.md) — the full grammar v1 currently copies.
- [`README.md`](README.md) — the versioning + coexistence policy.
- `docs/ARCHITECTURE.md` §4 — workflow run lifecycle.
