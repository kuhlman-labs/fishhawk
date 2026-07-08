# Workflow spec v1

Reference for `.fishhawk/workflows.yaml` at major version 1. The canonical schema is [`workflow-v1.schema.json`](workflow-v1.schema.json) (JSON Schema Draft 2020-12).

> **v1 began as a structural copy of v0 (ADR-046 / #1381) and now adds the deploy surface (E23.2 / #1382).** The inherited `$defs` and `properties` stay byte-for-byte identical to [`workflow-v0.schema.json`](workflow-v0.schema.json); v1 layers the delegating deploy grammar (per ADR-038 / #925) on top — the `deploy` stage type, the `deployment` artifact, the delegating executor, and three pre-flight constraint kinds. **v1.1 adds the `acceptance` stage type (E31.2 / #1519, per ADR-049)** — a runner-hosted advisory acceptance stage on the ordinary agent/human executor branches (no delegate, no deploy-only constraints); an additive minor, so every 1.0 spec stays valid. **v1.2 adds the `acceptance` produces artifact (E31.3 / #1531, per ADR-049)** — the durable acceptance-evidence record, valid only on an acceptance stage; also an additive minor. **v1.3 adds the acceptance-stage `egress` allowance (E31.4 / #1532, per ADR-050)** — the declared target host(s) the acceptance agent may reach through the runner's default-deny egress proxy; also an additive minor. **v0 stays frozen** and rejects both `deploy` and `acceptance` via its closed enums, so a v0 spec carrying either fails at the schema layer.

## Grammar

Every v0 field is inherited unchanged. For the full base reference (top-level shape, stages, executors, inputs, produces, constraints, budgets, gates, operator-agent delegation — including the `operator_agent.model_policy` scenario-A model-selection contract (#1421), inherited verbatim and surfaced identically on the run-status delegation block — decomposition controls), see [`workflow-v0.md`](workflow-v0.md). The v1 additions are the [deploy stage](#deploy-stage-v1) (v1.0), the [acceptance stage](#acceptance-stage-v11) (v1.1), the [acceptance artifact](#acceptance-artifact-v12) (v1.2), and the [egress allowance](#egress-allowance-v13) (v1.3) members below. A minimal non-deploy v1 spec differs from a v0 spec only in its `version` value:

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

## Acceptance stage (v1.1)

The `acceptance` stage type (ADR-049 / #1519) is a **runner-hosted advisory** acceptance stage: it runs a coding agent (or blocks on a human) to validate that a change meets its acceptance criteria, on the **same execution shape as `review`**. It deliberately adds **no new stage states** — it rides the existing agent-stage lifecycle (`pending → dispatched → running → awaiting_approval/succeeded/failed/cancelled`). That is the difference from `deploy`, whose two extra park states existed solely for its delegating pre-execution gate and external-pipeline poll. Acceptance does add one **optional, acceptance-stage-only** produces artifact — the [acceptance artifact](#acceptance-artifact-v12) (v1.2, E31.3 / #1531) — the durable evidence record of an acceptance run.

Because acceptance is an ordinary agent/human stage, it is bound by the **same type<->executor<->constraint rules the validator already applies to every non-deploy stage** (`backend/internal/spec/validate.go`), with no acceptance-specific code:

- An **acceptance stage MUST** use an `agent` or `human` executor. `executor.delegate` is deploy-only, so a delegating executor on an acceptance stage is rejected.
- The **pre-flight deploy constraint kinds** (`allowed_environments`, `change_freeze`, `required_upstream`) are **not** valid on an acceptance stage — they are deploy-only.
- The **`deployment` artifact** is **not** valid on an acceptance stage — it is deploy-only.
- The **`acceptance` artifact** (v1.2) is valid **only** on an acceptance stage — declaring it on any non-acceptance stage is rejected by the validator, the mirror of the `deployment`-off-a-deploy-stage rejection.

The E31.2 slice added the type to the spec, schema, and DB so an acceptance stage is **schema-valid and insertable**. E31.3 (#1531) adds the acceptance-evidence surface: the `acceptance` produces artifact (below), its persistence kind (`artifact.KindAcceptance`, migration 0045), and the `acceptance_*` living-anchor audit kinds. The gate/orchestration/runner semantics that execute an acceptance stage and populate the evidence are downstream (E31.6/E31.7); the stage is functionally inert until then.

### Example — an acceptance stage

```yaml
version: "1.1"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code # or: human: true
```

### acceptance artifact (v1.2)

The `acceptance` produces artifact (E31.3 / #1531) records the durable acceptance-evidence of an acceptance run — its runtime shape is `{verdict, per-criterion results, content_hash references to evidence blobs}`. It is **optional** and valid **only** on an acceptance stage (the validator rejects it on any other stage type, mirroring the `deployment`-off-a-deploy-stage binding). Like the `deployment` artifact, this schema slice only declares the artifact so an acceptance spec parses and validates; the runtime that populates it — capturing the verdict and per-criterion results into `artifact.KindAcceptance` (migration 0045) — is downstream (E31.6/E31.7).

```yaml
version: "1.2"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        produces:
          - artifact: acceptance
```

### egress allowance (v1.3)

The `egress` block (E31.4 / #1532, per ADR-050) declares the **target-instance host(s)** the acceptance agent may reach. It is valid **only** on an acceptance stage — the validator rejects it on any other stage type, the same binding shape as the acceptance artifact.

- `egress.target_hosts` (required, min 1): each entry is `host` or `host:port` — never a URL (the schema pattern rejects scheme/path/wildcard). An entry without a port permits the default HTTP/HTTPS ports (80, 443) only; an entry with a port permits exactly that port.
- These entries are the **single customer-controlled slot** of the acceptance agent's default-deny egress allow-list. The runner adds the model API endpoint and the Fishhawk backend itself; they are not declarable here.
- Enforcement is the runner-embedded egress proxy (`runner/internal/egressproxy`, ADR-050 decision #1): the acceptance invocation is forced through it via `HTTP(S)_PROXY`, destinations outside the composed allow-list are refused `403`, hostname resolutions are DNS-pinned against rebinding, and a public hostname resolving into loopback/private space is refused outright.
- The first `target_hosts` entry is also rendered into the acceptance prompt's Target instance section (`resolveAcceptanceTargetURL`) in full http(s) URL form — a schemeless `host`/`host:port` gains an `http://` prefix (e.g. `localhost:8080` → `http://localhost:8080`) so the validator is handed a URL (#1574). This URL-form prefix applies to the **prompt seam only**; the `egress.target_hosts` allow-list itself keeps the verbatim `host:port` grammar above. A spec with no `egress` block renders an explicit not-declared line instead.

```yaml
version: "1.3"
workflows:
  feature_change:
    stages:
      - id: acceptance
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts:
            - staging.example.com
            - preview.internal.example.com:8443
        produces:
          - artifact: acceptance
```

The three inline snippets above are minimal fragments. For a full runnable spec — a complete `feature_change` workflow whose `acceptance` stage exercises all three v1 minors together (type v1.1, artifact v1.2, egress v1.3) — see [`examples/workflow-v1-acceptance.yaml`](examples/workflow-v1-acceptance.yaml). That file is also the verbatim stanza the operator hand-applies to the live `.fishhawk/workflows.yaml` (the implement agent cannot touch `.fishhawk/**` — it is in `forbidden_paths`).

See [ADR-049 (#1519)](https://github.com/kuhlman-labs/fishhawk/issues/1519) for the acceptance-stage decision, [ADR-050 (#1540)](https://github.com/kuhlman-labs/fishhawk/issues/1540) for the egress + credential posture, and epic [#31](https://github.com/kuhlman-labs/fishhawk/issues/31) for the acceptance workstream.

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

## Approval gate predicate (v1)

An `approval` gate declares who must approve before it clears. v1 offers **two mutually exclusive** forms; a gate declares **exactly one**:

- `approvers` — the inherited GitHub-handle allow-list: named `roles` whose members (`@user` / `@org/team`) can satisfy the gate. Unchanged from v0.
- `approvals` — a **forge-neutral** approval predicate (E39.2 / #1707): the gate states its requirement without any repo-specific `@`-handle or top-level `roles` map.

The change is **strictly additive**: every existing `approvers`-only gate stays valid. An `approvals`-only gate is now legal too. The mutual exclusion is enforced **in the schema itself** (the gate approval-branch's inner `oneOf`), not merely in prose — a gate carrying **both** `approvers` and `approvals`, or **neither**, is rejected, so an approval gate is never a no-op and never ambiguously double-declared.

### `approvals` fields

| Field | Required | Shape | Meaning |
|---|---|---|---|
| `count` | **yes** | integer ≥ 1 | Number of distinct approvals to collect before the gate clears. Always explicit (ADR-055), so `approvals: {}` is rejected as a no-op. |
| `not` | no | array, unique, items `author` \| `agent` | Relationship classes barred from satisfying the gate — the change's own `author`, and any automated `agent` identity. Forge-neutral relationship classes, not handles. |
| `min_permission` | no (`x-intended-required`) | string enum `read` \| `triage` \| `write` \| `maintain` \| `admin` | Minimum forge-neutral repository permission tier an approver must hold, mirroring `backend/internal/identity.Permission` (the `none` tier is omitted — a `min_permission` of none is meaningless). |
| `member_of` | no (`x-intended-required`) | string (min 1) | A forge-neutral group (org or `org/team`) an approver must belong to. |
| `members` | no | array of strings (min 1) | Explicit approver subjects as **plain** forge-neutral strings — **not** the `@`-prefixed GitHub member-ref used by the legacy `roles.members` path — keeping the block forge-neutral. |

`min_permission` and `member_of` are annotated [`x-intended-required`](../../AGENTS.md#schema-change-checklist): optional now, intended to become required in a future major. `approvals` is accepted at every advertised v1 version (an additive optional field).

The three canonical presets (`docs/spec/workflow-preset-{low,medium,high}.yaml`) ship this handle-free form — their approval gates use `approvals: {count: 1, not: [author, agent]}` (ADR-055's ratified preset default), so a freshly-scaffolded repo has no `@your-github-handle` placeholder to replace.

```yaml
version: "1.0"
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
              min_permission: write
              member_of: my-org/reviewers
              members: [alice, bob]
```

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
