# Workflow spec v0

Reference for `.fishhawk/workflows.yaml`. The canonical schema is [`workflow-v0.schema.json`](workflow-v0.schema.json) (JSON Schema Draft 2020-12). Examples live in [`examples/`](examples/).

> **Frozen at Day 21** of the v0 build (MVP_SPEC.md §8). Old workflow runs in the audit log remain readable forever; never break this schema in place — bump to a new spec version (`workflow-v1`, `workflow-v2`...) instead.

### Schema evolution policy

`workflow-v0.x` is **additive-only**. New fields are optional; the existing required-field set is frozen. Validators must tolerate unknown fields (JSON Schema Draft 2020-12 §10.3).

**Breaking additions** (required new fields, renamed fields, removed fields, changed semantics of existing fields) require a version bump to `workflow-v1`. During the deprecation window, the backend accepts both `v0` and `v1` workflow specs simultaneously — the version string in the YAML routes each spec to its respective schema and execution path.

**`x-intended-required`** may annotate optional fields that are candidates for required promotion in `workflow-v1`. Annotation semantics and soak-period rules match those in `plan-standard-v1.md`.

## Top-level shape

```yaml
version: "0.3" # required; "0.3", "0.4" (adds workflow.budgets), "0.5" (adds operator_agent), "0.6" (adds workflow.decomposition.max_parallel), or "0.7" (adds the explicit advisory_reviewer_reject / gating_reviewer_reject page-event classes)
roles: # optional; named groups referenced by gates
  <role_id>:
    members: ["@org/team", "@user"]
test_conventions: [...] # optional; per-repo test-location rules for the plan-gate test sweep (#1004)
workflows: # required; at least one workflow
  <workflow_id>:
    description: "..."
    on_ci_failure: # optional; auto-retry policy (#276)
      max_retries: 1 # default when the block is absent
    budgets: [...] # optional; periodic per-workflow cost ceilings (ADR-030, v0.4+)
    drive: false # optional; auto-advance mechanical transitions (#1023)
    operator_agent: {...} # optional; delegation knobs for the operator agent (ADR-040, v0.5+)
    decomposition: # optional; decomposition controls (E24.6, v0.6+)
      max_parallel: 0 # 0 = unlimited; overrides FISHHAWKD_MAX_PARALLEL_CHILDREN
    stages: [...]
```

Identifiers (`<role_id>`, `<workflow_id>`, stage `id`s) are `snake_case` — `^[a-z][a-z0-9_]*$`. Member refs are GitHub conventions: `@user` for a single user, `@org/team` for a team. Resolution happens at run time against the GitHub App installation.

## Stages

```yaml
- id: plan
  type: plan | implement | review # closed set; no custom types
  executor: # exactly one of agent or human
    agent: claude-code        # any string; v0 ships claude-code (default) and codex
    model: claude-opus-4-8    # optional; per-stage model override (see ### Executor model override)
    timeout: 10m              # optional; stage-level override for agent timeout
    verify:                   # optional; in-band test gate (see ### Verify gate)
      command: 'scripts/test'
      timeout: '10m'
      max_iterations: 0       # optional; 0 = single-shot gate, >0 = bounded fix loop
    agent_self_retry: false   # optional; opt-in per ADR-023 (see ### Agent self-retry)
    # OR
    human: true
  inputs: [<input>...] # optional
  produces: [<artifact>...] # optional
  constraints: [<constraint>...] # optional, only meaningful for implement
  budget: <budget> # optional
  gates: [<gate>...] # optional
  reviewers: # optional; plan-review agents and human counts (ADR-027, see ### Plan reviewers)
    agent: 1  # integer >= 0; default 0
    human: 0  # integer >= 0; default 0
```

Stage `id` is unique within the workflow. The `from_stage` field on inputs cross-references it; the validator (E1.3 / #18) enforces this beyond the schema.

### Executor agent (provider)

`executor.agent` is a free-form string the runner resolves to a coding-agent adapter. v0 ships two providers: `claude-code` (the default — Anthropic's Claude Code CLI, reads `ANTHROPIC_API_KEY`) and `codex` (the OpenAI Codex CLI, reads `OPENAI_API_KEY`). Omitting `agent` selects `claude-code`; an unknown id fails the stage category-A before the agent is invoked. See the runner's [`Choosing the coding agent`](../../runner/README.md#choosing-the-coding-agent-claude-code-or-codex) section for the per-provider setup — the required GitHub secret, the action input wiring, and local/hosted verification. A worked example using Codex lives at [`examples/workflow-v0-codex-executor.yaml`](examples/workflow-v0-codex-executor.yaml).

**Migration note.** Existing Claude Code users need no changes: `agent` defaults to `claude-code` and behavior is byte-identical to before the provider-selection work landed (#839/#840/#841). Opting into Codex is a per-stage `executor.agent: codex` plus the `OPENAI_API_KEY` secret — nothing else in the spec changes.

### Verify gate

An optional in-band test gate that fires after the agent exits cleanly and before the bundle is committed. When `verify` is absent, the gate is skipped.

```yaml
executor:
  agent: claude-code
  verify:
    command: 'scripts/test'   # executed via sh -c; non-zero exit → category-A
    timeout: '10m'            # optional; defaults to 10m when absent
    max_iterations: 0         # optional; 0 (default) = single-shot demote-on-failure
```

The runner captures combined stdout+stderr into a `verify_run` bundle event so operators can inspect why the gate failed without re-running. Fields: `command`, `exit_code`, `output`, `outcome` (`"passed"` | `"failed"`).

`max_iterations` is the verify-fix loop budget (integer, minimum 0, default 0). `0` preserves today's single-shot demote-on-failure gate; `>0` enables a bounded evaluator-optimizer fix loop run against the committed scope-only tree, capping the total verify-fix agent invocations across the stage at this value. Worst-case wall clock is bounded by `(max_iterations+1) × executor.timeout` plus `(max_iterations+1) × verify.timeout`; `max_iterations` is the control surface and ties to the periodic cost budget (ADR-030). The value is delivered to the runner via `verify_max_iterations` in the prompt-fetch response (the runner's `--verify-max-iterations` flag overrides it).

The gate fires only when the agent itself succeeded (i.e., `res.OK` is true) — a failing agent already produces a category-A trace, and re-running the tests against a broken working tree would be misleading.

The full wiring path from spec to runner is deferred: v0 ships `verify` as a documented spec field for authoring surface. The runner reads the gate command from the `--verify-cmd` CLI flag; a follow-up issue will wire the backend to read `executor.verify.command` from the cached spec and deliver it to the runner via the prompt-fetch response.

### Agent self-retry

An opt-in boolean (default `false`, per ADR-023) that enables the agent to perform one self-initiated retry when it detects a recoverable failure, before the workflow's `on_ci_failure` policy kicks in.

```yaml
executor:
  agent: claude-code
  agent_self_retry: true  # opt in; default false
```

- **Only valid on agent-executed stages.** Declaring `agent_self_retry` on a `human: true` executor is a schema error — the field lives in the agent branch of the executor `oneOf`, so `unevaluatedProperties: false` rejects it on the human branch.
- **Backend plumbing and runner detection** are handled by separate follow-up tickets. This field documents the authoring surface and is parsed by both the backend and CLI validators; runner behavior is not yet wired.

### Executor model override

An optional per-stage model override (#1013). One rung of the implement-model resolution ladder.

```yaml
executor:
  agent: claude-code
  model: claude-opus-4-8  # optional; falls through to the next-lower rung when empty
```

- **Resolution ladder** (lowest to highest precedence): deployment default < `executor.model` < plan `model_recommendation.implement_model` < operator gate decision. The highest non-empty rung wins; an empty resolved model spawns the agent on the deployment default — byte-identical to today's behavior.
- **Gate-time validation.** The resolved model is validated against the deployment's per-adapter allowed-model set at the approval gate; an unknown model is rejected there, naming its source.
- **Only valid on agent-executed stages.** Declaring `model` on a `human: true` executor is a schema error — the field lives in the agent branch of the executor `oneOf`, so `unevaluatedProperties: false` rejects it on the human branch.
- **Additive within `workflow-v0.x`** — accepted at every advertised version; a spec that omits it parses unchanged.

### Plan reviewers

An optional block on `plan` stages that controls how many agent and/or human reviewers must weigh in before the stage advances to `awaiting_approval` (ADR-027). The block may be placed on any stage type but only has runtime effect on `plan` and `implement` stages (the implement-review loop reuses the same config and authority semantics).

```yaml
reviewers:
  agent: 1  # integer >= 0; 0 means no agent review (default)
  human: 0  # integer >= 0; 0 means no human approval gate (default)
```

**Heterogeneous agent reviewers (`agents`, #955):** instead of the bare `agent` count, a stage may declare one reviewer per list entry, each with its own provider and (optionally) model:

```yaml
reviewers:
  agents:
    - provider: anthropic        # anthropic | claudecode | codex
      model: claude-opus-4-8     # optional; empty → provider's deployment default
    - provider: codex
      model: gpt-5.5
  human: 1
```

- **Supersession rule:** when `agents` is present and non-empty it supersedes the bare `agent` integer — the effective agent count is `len(agents)` (`spec.ReviewersConfig.AgentCount()`). The integer form remains valid and unchanged for back-compat: `agent: N` invokes the deployment's precedence-selected default adapter N times.
- **Authority is count-derived (ADR-027 unchanged):** the authority table below reads the *effective* count, so heterogeneity changes **who** reviews, not gating semantics. `agents` + `human: 0` is gating; `agents` + `human: 1` is advisory.
- **Provider resolution:** each provider must be configured in the deployment (`FISHHAWKD_ANTHROPIC_API_KEY` / `FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER` / `FISHHAWKD_ENABLE_CODEX_REVIEWER`). A **gating** stage naming an unconfigured provider fails dispatch up front at run create (`plan_reviewer_unconfigured`). In **advisory** mode an unresolvable provider degrades per-reviewer: a `plan_review_failed` / `implement_review_failed` audit entry carries the resolve error and the loop continues with the remaining reviewers.
- The self-review guard runs per-invocation against each reviewer's returned model; codex reasoning effort stays a deployment-level knob (the spec carries provider + model only).

**Authority modes** (resolved by `planreview.ResolveAuthority`):

| `agent` | `human` | Authority mode | Effect |
|---------|---------|----------------|--------|
| `> 0`   | `== 0`  | **gating**     | Agent rejections block stage advancement to `awaiting_approval`. All agent reviews must approve (or approve_with_concerns) before the plan advances. |
| `> 0`   | `> 0`   | **advisory**   | Agent verdicts are surfaced in `fishhawk_get_plan` and recorded as `plan_reviewed` audit entries, but cannot block human approval. |
| `== 0`  | any     | **gateless**   | No agent review. Human approval (if `human > 0`) proceeds as before. |

**Default behavior (absent `reviewers` block):** When the `reviewers` field is omitted, the backend treats the stage as `{human: 1}` — preserving the existing one-human-approver behavior from before ADR-027. Callers reading `Stage.Reviewers == nil` must apply this default.

**Self-review guard:** If the review agent's model identifier matches the plan's `generated_by.model`, the server logs a WARN but does not block. ADR-027 treats this as an advisory signal only.

**`plan_reviewed` audit category:** Each agent review invocation appends a `plan_reviewed` entry to the run's audit log with `reviewer_kind: "agent"`, the structured verdict (`approve`, `approve_with_concerns`, `reject`), and any concerns. The verdict is surfaced via `fishhawk_get_plan` in the `Reviews` field.

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

## Budget (per-stage)

```yaml
budget:
  max_tokens: 200000
  max_runtime_minutes: 15
  enforcement: advisory | blocking
```

A per-stage cap on token / runtime usage for a single stage execution. v0 ships `advisory` enforcement only — the runner reports overruns but does not abort. `blocking` arrives in v0.x once Fishhawk issues ephemeral agent keys (so the proxy can hard-cap).

This is distinct from the workflow-level `budgets` below: the per-stage `budget` governs one stage's resource use; `budgets` govern aggregate USD spend across runs.

## Periodic budgets (workflow-level, v0.4+)

A workflow-level list of recurring cost ceilings (ADR-030 / #688). Each entry caps total USD spend across **all runs** of the workflow within a calendar period, resetting at the period boundary. Requires `version: "0.4"`.

```yaml
workflows:
  feature_change:
    budgets:
      - period: weekly | monthly   # calendar reset cadence
        limit_usd: 50              # ceiling in USD for the period (> 0)
        enforcement: advisory | blocking  # optional; defaults to advisory
        warn_at: 0.8               # optional fraction [0,1]; warn at 80% before 100%
    stages: [...]
```

- **`period`** — `weekly` resets at the start of the ISO week; `monthly` resets on the first of the month. Boundaries are timezone-aware.
- **`limit_usd`** — the ceiling, summed from `runs.cost_usd_total` (#649/#680/#684) across the workflow's runs created within the current period.
- **`enforcement`**:
  - `advisory` (default) — a `budget_alert` audit entry + issue comment fires when period spend crosses `warn_at` and again at 100%. Runs never block.
  - `blocking` — a **new** run is refused at admission once the period spend exhausts `limit_usd`. In-flight runs are never touched (the gate is admission-only); an operator can override to force a run past the ceiling.
- **`warn_at`** — optional fraction in `[0,1]` (e.g. `0.8` for 80%) at which the advisory warning fires ahead of the 100% crossing. Absent means only the 100% threshold is surfaced.

**Advisory surfacing (#688).** For an `advisory` budget the backend re-evaluates the workflow's period spend after every cost-bearing trace upload (in `trace.go::checkBudgetAlerts`, after the per-run cost rollup increments). On a `warn_at` crossing and again on a 100% crossing it appends a `budget_alert` audit entry and posts an advisory comment on the triggering issue, each deduped so the warn tier and the 100% tier fire at most once per calendar period. Period boundaries are computed in the backend's `FISHHAWKD_BUDGET_TIMEZONE` (default UTC). The surface is warn-only and best-effort: it never gates, fails, or blocks a run. `blocking` enforcement (admission-time refusal) is not yet implemented and is tracked as a follow-up; until then a `blocking` budget records spend but produces no automatic refusal.

> Cost honesty caveat: `known_usage=false` bundles undercount spend (#685), so a ceiling may be crossed later than true spend would imply. Period spend is summed from the recorded `runs.cost_usd_total`, a lower bound on actual spend — the advisory comment repeats this caveat so a reader doesn't treat the figure as exact.

## Gates

Two types:

```yaml
# Approval gate — blocks until an approver acts.
- type: approval
  approvers:
    any_of: [tech_lead, senior_engineer] # or all_of
  sla: 4_business_hours # optional; D-category timeout
  operator_agent: {...} # optional; per-gate delegation override (ADR-040, v0.5+; see ## Operator agent delegation)

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

## Drive mode

```yaml
workflows:
  feature_change:
    drive: true # optional; default false
    stages: […]
```

Opt-in auto-advancement of mechanical run transitions (#1023 / #996 theme 1). When `drive: true`, fishhawkd advances the transitions that carry no judgment content — plan-approved → implement dispatch, review verdicts settling a gate, fixup-pushed re-review re-park, all-gates-resolved + checks-green parking at a derived `awaiting_merge` — and records a `run_auto_advanced` audit entry naming the transition rule for each advance. Judgment points (gate approvals, concern routing, merge) always park for the operator.

- **`drive`** — boolean, default `false`. The workflow-level value is the default for every run of the workflow; `POST /v0/runs` accepts a per-run `drive` override that wins over the spec value. The resolved flag is persisted on the run row (`runs.drive`) at create time, so a spec edit mid-run doesn't change an in-flight run's behavior.
- **Additive within workflow-v0.x** — optional field, no version bump; specs without it parse unchanged.
- The flag is persisted-but-inert until the drive engine lands (`backend/internal/drive`, sibling slice of #1023): nothing consumes it at the spec layer beyond parsing and run-create resolution.

## Operator agent delegation (v0.5+)

Delegation knobs for the operator agent (ADR-040 / #1026). Each `may_*` knob names the **single backend-evaluable condition** under which the operator agent may take that action without paging the human. The per-knob values are closed single-entry enums in v0 — a condition exists only if the backend can answer it from run state.

```yaml
workflows:
  feature_change:
    operator_agent: # workflow-level default for every gate
      may_approve: clean_dual_approval
      may_route_fixup: convergent_concerns
      may_waive: solo_low
      may_retry: infra_flake
      may_merge: gates_resolved_ci_green
      must_page_human: [reviewer_reject, budget_override]
    stages:
      - id: plan
        type: plan
        executor: { agent: claude-code }
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers: { any_of: [founder] }
            operator_agent: # per-gate override; wins WHOLESALE over the workflow block
              may_approve: clean_dual_approval
```

**Per-knob conditions** (closed v0 set):

| Knob | Condition | Met when |
|---|---|---|
| `may_approve` | `clean_dual_approval` | every configured reviewer for the gated stage returned an approve verdict AND zero concerns are open |
| `may_route_fixup` | `convergent_concerns` | all reviewer verdicts are in, at least one concern is open, and no **gating-authority** reviewer rejected — under advisory authority (agent + human reviewers, ADR-027) an agent reject is arbitrable and does NOT disqualify; under gating authority (agent-only) a reject fires `reviewer_reject` and pages |
| `may_waive` | `solo_low` | exactly one open concern and its severity is low |
| `may_retry` | `infra_flake` | the latest stage failure is classified as an infrastructure flake |
| `may_merge` | `gates_resolved_ci_green` | no pending gate approvals, zero open concerns, PR open, required checks green |

**Fail-closed default.** An absent `operator_agent` block delegates nothing: every judgment pages the human, exactly as before v0.5. Likewise an absent knob within a block — only the named knobs are delegated. Specs without the block behave byte-identically to today.

**Precedence.** A gate-level block (approval gates only — the schema rejects `operator_agent` on `check` gates) overrides the workflow-level block **wholesale**: knobs are never merged across levels, so a gate block that omits `may_retry` does not inherit the workflow's `may_retry`. Resolution lives in `spec.Workflow.EffectiveOperatorAgent(gate)`: gate block if present, else workflow block, else nil.

**`must_page_human`.** Events that always page the human regardless of the `may_*` knobs (closed set: `reviewer_reject`, `advisory_reviewer_reject`, `gating_reviewer_reject`, `plan_rejection`, `scope_amendment`, `budget_override`, `policy_override`, `exception_request`, `requirement_arbitration`, `clarification_request`). An event listed here is never absorbed by a delegation. (`clarification_request` is the planner parking the plan stage at `awaiting_input` because the issue is not yet plannable — #1057.)

**Reviewer-reject taxonomy (v0.7+).** The reviewer-reject class is now self-documenting via two explicit tokens (#1378): `gating_reviewer_reject` — an agent reject on an agent-only implement review, where no human approver overrides it (ADR-027); this **pages** the human — and `advisory_reviewer_reject` — an agent reject under agent + human authority; this is non-blocking and **arbitrable** via `may_route_fixup` / `convergent_concerns`, so it does not page on its own. The legacy bare `reviewer_reject` is preserved and **resolves to the gating sense** for back-compat: a spec using it pages exactly as before. The page/auto decision itself stays resolved from ADR-027 review authority (`planreview.ResolveAuthority`) — the explicit tokens only make the resolved class legible in config; they do not change behavior. A **human** reviewer reject does not arrive as an `implement_reviewed` verdict at all — it surfaces as `plan_rejection` / gate rejection, which already pages.

The class a run currently resolves to is surfaced on the wire as `reviewer_reject_class` on the `GET /v0/runs/{id}` delegation block (`gating_reviewer_reject` / `advisory_reviewer_reject`, omitted when the implement stage is gateless), so a reader need not cross-reference the authority resolver.

**Authority unchanged (ADR-027).** Delegation changes *who* may act at a gate, not what gates exist or how reviewer authority resolves. The condition evaluator reads authority from the same ADR-027 mechanism the review pipeline uses (`planreview.ResolveAuthority`: advisory when agent + human, gating when agent-only), so a delegated decision can never weaken a gating gate — it only loosens delegation where ADR-027 already makes agent verdicts non-blocking.

- `may_merge` is evaluated and surfaced but has no backend merge endpoint to enforce in v0 — merge happens on GitHub; enforcement attaches when a merge action surface exists.

**Decision-class taxonomy.** Each class of operator decision resolves to either an **auto** action (delegated via a `may_*` knob when its condition is met) or a **page** (a `must_page_human` event). The operator agent reads this mapping from config rather than judging the advisory-vs-hard line per-run.

| Decision class | Resolution | Via |
|---|---|---|
| Clean dual approval (all verdicts approve, no concerns) | auto | `may_approve` / `clean_dual_approval` |
| Advisory-concern arbitration (verdicts in, no gating reject, concern open) | auto | `may_route_fixup` / `convergent_concerns` |
| Advisory reviewer reject (agent reject under agent + human authority) | auto | `may_route_fixup` / `convergent_concerns` (arbitrable; non-blocking per ADR-027); legible token `advisory_reviewer_reject` |
| Gating reviewer reject (agent reject under agent-only authority) | page | `gating_reviewer_reject` (legacy `reviewer_reject` resolves to this sense) |
| Human / hard reject | page | `plan_rejection` (gate rejection — never an `implement_reviewed` verdict) |
| Scope amendment (agent requests new in-scope paths) | page | `scope_amendment` |
| Budget / policy override | page | `budget_override`, `policy_override` |
| Plan design-fork / clarification gate (plan parked at `awaiting_input`) | page | `clarification_request`, `requirement_arbitration`, `exception_request` |
| Merge (gates resolved, CI green) | auto | `may_merge` / `gates_resolved_ci_green` (surfaced-only in v0; no enforce endpoint) |

## Decomposition controls (v0.6+)

A workflow-level `decomposition` block (E24.6 / #1146) holding decomposition controls. v0.6 ships a single knob:

```yaml
version: "0.6"
workflows:
  feature_change:
    decomposition:
      max_parallel: 3 # 0 = unlimited
    stages: […]
```

- **`max_parallel`** — integer, `minimum: 0`. The maximum number of decomposed child runs that may dispatch **concurrently** for a run of this workflow. `0` (and an absent block) means **unlimited**. It is a per-workflow override of the global `FISHHAWKD_MAX_PARALLEL_CHILDREN` operator default: when `max_parallel > 0` the knob wins, otherwise the global default applies. Resolution lives in `spec.Workflow.EffectiveMaxParallel(globalDefault)`, and `0 = unlimited` is kept consistent with `budget.ParallelDecision`'s cap semantics.
- **Mechanism vs. enforcement.** v0.6 declares the cap and the orchestrator **resolves and surfaces** it (a log line plus `effective_max_parallel` in the `plan_decomposed` audit payload). It does **not** yet throttle the fan-out — every child is still minted. The concurrency throttle that consumes the resolved cap lands in E24.3 (#1143).
- **Additive within workflow-v0.x** — optional field, no new required field, no major bump (no `x-intended-required`). Specs without it parse unchanged at every advertised `version`.

### Notes — soak window

`workflow.decomposition.max_parallel` is introduced as a new **optional** field within `workflow-v0.x` (version enum gains `0.6`). It is **not** slated to become required, so no `x-intended-required` annotation is set and no required-promotion soak is owed. The compatibility soak here is the standard additive-change window: during it the backend validates both the prior schema versions (`0.3`–`0.5`) and `0.6`, so specs authored against either continue to parse. Duration is per-PR; no minimum is set yet (TBD in the follow-up tracked by the AGENTS.md schema-change checklist).

## Test conventions (#1004)

An optional **top-level** `test_conventions` array that makes test-location rules per-repo data for the plan-gate test sweep (`backend/internal/server/test_sweep.go`). Each entry maps production files matching a glob to candidate test-file path templates; the sweep flags a candidate test that **exists on the base ref but is missing from the plan's `scope.files`** (the class the runner would otherwise `scope_drift`-exclude). Advisory-only and fail-open — it never blocks a plan.

```yaml
version: "0.3"
test_conventions:
  - match: "src/**/*.py" # doublestar glob; ** crosses directory separators
    candidates:
      - "tests/test_{name}.py"
  - match: "lib/**/*.rb"
    candidates:
      - "spec/{relpath}_spec.rb"
workflows:
  feature_change:
    stages: […]
```

- **`match`** — a [doublestar](https://github.com/bmatcuk/doublestar) glob (`**` crosses `/`, unlike `path.Match`) matched against the repo-relative production-file path. A scoped file whose basename matches a candidate's test-file shape is treated as a test, not a production file.
- **`candidates`** — one or more test-file path templates (`minItems: 1`) for a matched production file. Template variables:
  - `{dir}` — the production file's directory (`path.Dir`)
  - `{name}` — basename without its final extension (`upload` for `upload.go`)
  - `{ext}` — final extension without the leading dot (`tsx` for `Foo.tsx`)
  - `{relpath}` — the full repo-relative path without its final extension (`lib/foo/bar` for `lib/foo/bar.rb`)
- **Built-in defaults, always on.** The sweep ships defaults reproducing the Go rule (`**/*.go` → `{dir}/{name}_test.go`) and colocated TypeScript (`**/*.{ts,tsx}` → `{dir}/{name}.test.{ext}`, `{dir}/{name}.spec.{ext}`, `{dir}/__tests__/{name}.test.{ext}`). Declared conventions **append** to these — they never replace them — so a repo declaring only Python/Ruby keeps Go + colocated TS covered, and a spec with no `test_conventions` is byte-identical to the pre-#1004 sweep.
- **Additive within workflow-v0.x** — optional top-level field, no version bump; accepted at every advertised `version`. Specs without it parse unchanged.

## Identifier namespaces

| Field                       | Pattern / values                                                             | Notes                                                                                               |
| --------------------------- | ---------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `test_conventions[].match`  | non-empty doublestar glob                                                    | per-repo test-location rule for the plan-gate test sweep (#1004); `**` crosses `/`; additive to built-in Go + TS defaults |
| `test_conventions[].candidates` | array of non-empty path templates, `minItems: 1`                         | template vars `{dir}` / `{name}` / `{ext}` / `{relpath}`                                            |
| `version`                   | `"0.3"` \| `"0.4"` \| `"0.5"` \| `"0.6"` \| `"0.7"`                          | 0.7 adds the explicit `advisory_reviewer_reject` / `gating_reviewer_reject` page-event classes (#1378); 0.6 adds `decomposition.max_parallel` (#1146); 0.5 adds `operator_agent` (ADR-040 / #1026); 0.4 adds workflow-level `budgets` (ADR-030 / #688); 0.3 adds `on_ci_failure.max_retries` (#277); 0.2 dropped `blocking_checks` |
| `budgets[].period`          | `weekly` \| `monthly`                                                        | workflow-level periodic budget reset cadence (v0.4+)                                                |
| `budgets[].enforcement`     | `advisory` \| `blocking`                                                     | advisory warns; blocking refuses a new run at admission                                             |
| Role / workflow / stage IDs | `^[a-z][a-z0-9_]*$`                                                          | snake_case                                                                                          |
| Member refs                 | `^@[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)?$`                                    | GitHub user or team                                                                                 |
| Stage `type`                | `plan` \| `implement` \| `review`                                            | closed set                                                                                          |
| Executor                    | `agent: <string>` xor `human: true`                                          | mutually exclusive                                                                                  |
| `executor.model`            | non-empty string (optional)                                                  | per-stage model override (#1013); agent branch only, schema error on human executor; additive at every version |
| `executor.agent_self_retry` | `true` \| `false` (default `false`)                                          | agent branch only; schema error on human executor                                                   |
| `reviewers.agent`           | integer `>= 0` (default `0`)                                                 | absent block → nil → backend defaults to `{human:1}`; superseded by a non-empty `reviewers.agents` |
| `reviewers.agents`          | array of `{provider, model?}`, `minItems: 1`                                 | heterogeneous reviewers (#955); when present, effective agent count = `len(agents)`                |
| `reviewers.agents[].provider` | `anthropic` \| `claudecode` \| `codex`                                     | must be configured in the deployment; gating + unresolvable provider fails dispatch up front       |
| `reviewers.agents[].model`  | string (optional)                                                            | empty → the provider's deployment-configured default model                                         |
| `reviewers.human`           | integer `>= 0` (default `0`)                                                 | absent block → nil → backend defaults to `{human:1}`                                               |
| Input `source`              | `github_issue` \| `pull_request`                                             | v0; v0.x adds Linear/Jira                                                                           |
| Artifact                    | `plan` \| `pull_request`                                                     | closed set                                                                                          |
| Persistence target          | `originating_issue` \| `fishhawk_audit_log`                                  | closed set                                                                                          |
| Persistence mode            | `rendered_comment` \| `canonical`                                            | closed set                                                                                          |
| Constraint kind             | `max_files_changed`, `forbidden_paths`, `allowed_paths`, `required_outcomes` | exactly one per constraint                                                                          |
| `required_outcomes` items   | `tests_added_or_updated`, `ci_green`                                         | closed set                                                                                          |
| Budget enforcement          | `advisory` \| `blocking`                                                     | v0 ships advisory only                                                                              |
| Gate `type`                 | `approval` \| `check`                                                        | closed set                                                                                          |
| Approvers shape             | `any_of: [<role_id>...]` xor `all_of: [<role_id>...]`                        | one shape per gate                                                                                  |
| `operator_agent.may_*`      | one closed condition per knob (see ## Operator agent delegation)             | v0.5+; workflow level + approval-gate override (gate wins wholesale); absent → fail-closed          |
| `operator_agent.must_page_human` | `reviewer_reject`, `advisory_reviewer_reject`, `gating_reviewer_reject`, `plan_rejection`, `scope_amendment`, `budget_override`, `policy_override`, `exception_request`, `requirement_arbitration`, `clarification_request` | closed set; always pages the human regardless of `may_*` knobs. The explicit reject classes (v0.7+) make the taxonomy self-documenting; legacy `reviewer_reject` resolves to the gating sense |

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
