# Plan artifact `standard_v1`

The output of every `type: plan` stage. Validated by the runner (E5.4 / #31) before a stage is considered successful, persisted into the audit log as the canonical record of agent intent, and rendered as a comment on the originating GitHub issue.

The canonical schema is [`plan-standard-v1.schema.json`](plan-standard-v1.schema.json). An example plan that validates lives at [`examples/plan-standard-v1-example.json`](examples/plan-standard-v1-example.json).

> **Frozen at Day 21** of the v0 build. Old plans in the audit log remain readable forever — never break this schema in place. Future versions land as `standard_v2` etc., and the plan validator (E1.5 / #20) keeps every version readable.

## Top-level shape

```json
{
  "plan_version": "standard_v1",

  "ticket_reference": { "type": "...", "url": "...", "id": "..." },
  "generated_by":     { "agent": "...", "model": "...", "timestamp": "..." },

  "summary":      "...",
  "scope":        { "files": [...], "estimated_lines_changed": 0 },
  "approach":     [ { "step": 1, "description": "..." }, ... ],
  "verification": { "test_strategy": "...", "rollback_plan": "..." },

  "predicted_runtime_minutes":    20,
  "predicted_runtime_confidence": "medium",

  "risks_and_assumptions": ["..."],
  "decomposition": { "rationale": "...", "sub_plans": [...] }
}
```

`plan_version` is required and pins the document to this schema. The validator routes versions to their respective schemas; an unknown version is a category-A failure.

## Required fields

### `ticket_reference`

```json
{
  "type": "github_issue",
  "url":  "https://github.com/kuhlman-labs/fishhawk/issues/1247",
  "id":   "kuhlman-labs/fishhawk#1247"
}
```

`type` is `github_issue` in v0 (closed set; v0.x adds Linear and Jira). `url` is the canonical web URL; `id` is a stable identifier suitable for indexing.

### `generated_by`

```json
{
  "agent":     "claude-code",
  "model":     "claude-opus-4-7",
  "version":   "build-abc123",
  "timestamp": "2026-04-30T14:22:11Z"
}
```

`agent` matches the workflow spec's `executor.agent`. `model` is the specific model. `timestamp` is RFC 3339, recorded at agent invocation. `version` is optional and used when the agent surfaces a build SHA.

### `summary`

A non-empty human-readable description. The plan-review UI renders this as the lead paragraph; the GitHub issue comment uses it as the headline. One to three sentences.

### `scope`

```json
{
  "files": [
    { "path": "backend/internal/api/audit_export.go", "operation": "create" },
    { "path": "backend/internal/api/router.go",        "operation": "modify" }
  ],
  "estimated_lines_changed": 250
}
```

`files` lists every file the agent intends to touch with one of `create | modify | delete`. The runner's post-hoc constraint check (E5.5 / #53) compares this list against the actual diff and against the stage's `forbidden_paths` / `allowed_paths` constraints.

`estimated_lines_changed` is a reviewer cue, not enforced.

### `approach`

Ordered list of steps. Each step has a 1-indexed `step` number and a `description`. At least one step is required. Steps are how reviewers grok intent quickly; the runner does not consume them programmatically.

```json
[
  { "step": 1, "description": "Add /v0/audit/export endpoint with date-range filters" },
  { "step": 2, "description": "Stream JSON Lines from audit_entries with cursor-based pagination" },
  { "step": 3, "description": "Add an integration test that seeds 1000 entries and asserts ordering" }
]
```

### `verification`

```json
{
  "test_strategy": "Integration test: seed 1000 audit entries across 3 dates, exercise --from/--to filters, assert pagination cursor is stable.",
  "rollback_plan": "Revert the PR. Endpoint is additive and reads existing tables; no data migration to roll back."
}
```

Reviewers expect concrete tests, not "add tests." Rollback plans flag whether a change is purely additive or has data migration consequences.

### Verification with tiered checkpoints

Plans that include expensive test gates must allocate wall-clock time for them in `predicted_runtime_minutes`. The agent should name cheap per-batch checks separately from the expensive final pass:

```json
{
  "test_strategy": "After each batch of changes: run unit tests for the touched package only (e.g. `go test ./internal/foo/...`). Final iteration when implementation is complete: run the full flake check (`go test -count 100 -race ./...`). The expensive final pass is estimated at 15 minutes and is included in predicted_runtime_minutes."
}
```

Expensive gates are: `-count >= 50`, or `-race` combined with `./...` (full-repo). These are reserved for the final iteration. The advisory heuristic in `plan.Warnings` surfaces a warning when an expensive gate appears in `test_strategy` but `predicted_runtime_minutes` is below 20, flagging plans where the runtime budget is implausibly short for the stated verification approach.

### Runtime prediction

Two required fields capture the agent's estimate of how long the implement stage will take.

#### `predicted_runtime_minutes`

Integer ≥ 1. The agent's estimate in minutes. Used to surface scope problems early: if the estimate exceeds the implement-stage budget (per ADR-025), the agent must also populate `decomposition.sub_plans`.

#### `predicted_runtime_confidence`

One of `"low"`, `"medium"`, or `"high"`.

| Value | Meaning |
|---|---|
| `low` | Rough guess; significant unknowns remain |
| `medium` | Reasonably grounded; agent has read the relevant code |
| `high` | Well-understood scope; agent has high certainty |

These fields are MUST-populate: every `standard_v1` artifact must carry an estimate. The plan-stage prompt instructs the agent accordingly (ADR-025 D1 framing).

## Optional fields

### `risks_and_assumptions`

```json
[
  "Assumes audit_entries.payload_jsonb has indexed fields the filter uses",
  "Risk: large date ranges could be expensive; mitigated by a cursor-based limit of 1000 rows per page"
]
```

Free-form strings. The plan-review UI surfaces these in a sidebar. Useful for the agent to flag uncertainty rather than over-claim confidence.

### Decomposition

Populated when `predicted_runtime_minutes` exceeds the implement-stage budget. Signals that the agent believes the work should be split across multiple runs.

```json
{
  "decomposition": {
    "rationale": "Estimated runtime (90 min) exceeds the 60-minute implement-stage budget. Splitting into schema migration (Part A) and application logic + tests (Part B).",
    "sub_plans": [
      {
        "title": "Part A: schema migration",
        "scope_hint": "Add the new columns and indexes; no application code changes.",
        "predicted_runtime_minutes": 20,
        "predicted_runtime_confidence": "high"
      },
      {
        "title": "Part B: application logic and tests",
        "scope_hint": "Wire up the new columns in service layer and add integration tests.",
        "predicted_runtime_minutes": 55,
        "predicted_runtime_confidence": "medium"
      }
    ]
  }
}
```

#### `decomposition.rationale`

Required when `decomposition` is present. Explains why the work was split and how the sub-plans relate.

#### `decomposition.sub_plans`

Required array with at least two entries. Each entry is a `SubPlanSummary`:

| Field | Type | Required | Notes |
|---|---|---|---|
| `title` | string (1–200 chars) | yes | Must be unique within the array |
| `scope_hint` | string | yes | What this sub-plan covers |
| `predicted_runtime_minutes` | integer ≥ 1 | yes | Estimate for this sub-plan's implement stage |
| `predicted_runtime_confidence` | `"low"` / `"medium"` / `"high"` | yes | Confidence in the sub-plan estimate |

**Runtime-sum invariant**: the validator warns (but does not reject) when the sum of `sub_plans[*].predicted_runtime_minutes` is less than the parent `predicted_runtime_minutes`. The agent may legitimately compress work when breaking it into smaller pieces; the soft warning surfaces the gap for human review.

**Lifecycle**: as of ADR-025 D4 (#455), `decomposition` is acted upon by the orchestrator. After plan approval, when the orchestrator's `Advance` would dispatch the parent's implement stage, it checks the approved plan: if `decomposition.sub_plans` is populated, the orchestrator mints one child run per sub-plan (each carrying `parent_run_id = parent.id` and `decomposed_from = parent.id`, with an issue_context built from the parent's title plus the sub-plan's `scope_hint`), parks the parent's implement stage in `awaiting_children`, and emits a `plan_decomposed` audit entry listing the child IDs. The child-completion sweeper (`backend/internal/childcompletion/`) transitions the parent stage to `succeeded` once every child reaches a terminal state successfully, or to `failed-C` if any child failed. Child runs themselves skip the fanout check (their `decomposed_from` is non-nil), so recursion is bounded at one level.

## Validation rules beyond the schema

JSON Schema enforces structure. The validator (E1.5 / #20) layers on:

- `scope.files[].path` matches at least one of the stage's `allowed_paths` (when set) and none of the `forbidden_paths`.
- `generated_by.timestamp` is within the run's wall-clock window (catches clock-skew or replay).
- `generated_by.agent` matches the workflow spec's `executor.agent` for the active stage.
- `decomposition.sub_plans[*].title` must be unique within the array (semantic check in the plan package; returns `*SemanticError` on violation).

These cross-references aren't expressible in JSON Schema cleanly.

## Persistence

Per `MVP_SPEC.md` §4.3:

| Surface | Mode | Notes |
|---|---|---|
| `fishhawk_audit_log` | `canonical` | Full structure, immutable. The audit log is the source of truth. |
| `originating_issue`  | `rendered_comment` | Rendered Markdown comment on the GitHub issue. Updated if the plan is regenerated. |

The runner ships the canonical JSON to the backend; the backend renders the Markdown view and posts it via the GitHub App's installation token.

## See also

- `MVP_SPEC.md` §4.3 — original specification of required and optional fields.
- `MVP_SPEC.md` §4.4 — audit log persistence.
- `workflow-v0.md` — the workflow spec that produces these artifacts.
- ADR-025 — stage budget framing and the `predicted_runtime_minutes` requirement.
