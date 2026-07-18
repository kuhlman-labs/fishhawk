# Plan artifact `standard_v1`

The output of every `type: plan` stage. Validated by the runner (E5.4 / #31) before a stage is considered successful, persisted into the audit log as the canonical record of agent intent, and rendered as a comment on the originating GitHub issue.

The canonical schema is [`plan-standard-v1.schema.json`](plan-standard-v1.schema.json). An example plan that validates lives at [`examples/plan-standard-v1-example.json`](examples/plan-standard-v1-example.json).

> **Frozen at Day 21** of the v0 build. Old plans in the audit log remain readable forever — never break this schema in place. Future versions land as `standard_v2` etc., and the plan validator (E1.5 / #20) keeps every version readable.

### Schema evolution policy

`standard_v1.x` is **additive-only**. New fields must be optional; the existing required-field set is frozen. Clients that validate against `standard_v1` must tolerate unknown fields (they are collected as annotations per JSON Schema Draft 2020-12 §10.3 and must not cause validation failure).

**Required-field promotion** requires a major version bump (`standard_v2`). During the deprecation window, the plan validator accepts both `standard_v1` and `standard_v2` artifacts and routes each to its respective schema. The backend advertises which versions it can validate via the `/healthz` schema-versions endpoint (once #466 lands).

**`x-intended-required` annotation** — a non-standard JSON Schema keyword used to signal that a field is currently optional but is a candidate for required promotion in the next major version. Example:

```json
"some_field": {
  "type": "string",
  "x-intended-required": true
}
```

This annotation does not affect validation (see §10.3). It is a contract signal for schema authors: the soak period during which the field is optional must be declared in the introducing PR body before the required promotion is merged.

**`x-coerce-principal` / `x-coerce-defaults` annotations** — non-standard JSON Schema keywords on `$defs` entries that opt a definition into server-side coercion. When a bare string appears where the schema expects an object, `TryCoerce` uses these annotations to reconstruct the object automatically:

- `x-coerce-principal` (string): the property name that receives the bare string value.
- `x-coerce-defaults` (object): default values for all other required properties. The sentinel `"<<runtime:now>>"` on any string value is replaced with the upload timestamp at runtime.

Example — adding coercion to a new `$defs` entry:

```json
"my-object": {
  "type": "object",
  "required": ["name", "kind"],
  "x-coerce-principal": "name",
  "x-coerce-defaults": {"kind": "default"},
  "properties": { ... }
}
```

Any property whose `$ref` (or array `items.$ref`) points to an annotated `$defs` entry is automatically registered at package init — no code change to `TryCoerce` is required. Both `x-coerce-principal` and `x-coerce-defaults` are collected as annotations per JSON Schema Draft 2020-12 §10.3 and do not affect validation.

**Removals** follow a longer deprecation window — duration TBD. No fields have been removed from `standard_v1`.

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
  "decomposition": { "rationale": "...", "sub_plans": [...] },
  "model_recommendation": { "implement_model": "...", "rationale": "...", "complexity_assessed": "low|medium|high" },
  "surface_sweep_exemptions": [ { "pattern": "...", "sibling": "...", "reason": "..." } ],
  "over_cap": false,
  "split_proposal": { "rationale": "...", "phases": [ { "title": "...", "scope": { "files": [...] }, "depends_on": [] }, ... ] }
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

`scope.files` is also read by the implement-review agent (ADR-027 impl 2/2) as a **flag-only drift signal**: when the implement-stage diff touches files outside `scope.files`, the review agent emits a `{category: "scope"}` concern naming the out-of-scope files — it does **not** auto-reject. Only an overall verdict of `reject` (a blocking plan/correctness problem) blocks stage advancement under gating authority; scope drift alone surfaces as a concern for the operator to weigh.

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

`verification` also carries two optional, additive fields within `standard_v1` (ADR-049 #3, E31 wave 0):

```json
{
  "test_strategy": "...",
  "rollback_plan": "...",
  "acceptance_criteria": [
    {
      "id": "bulk-export-paginates",
      "statement": "GET /v0/audit/export returns a stable cursor across pages.",
      "source": "explicit",
      "source_ref": "kuhlman-labs/fishhawk#1247"
    },
    {
      "id": "large-range-bounded",
      "statement": "A very large date range is bounded by a per-page row limit.",
      "source": "inferred",
      "rationale": "The ticket implies unbounded ranges are a risk; inferred to guard memory.",
      "blocking": false,
      "verify_hint": "Assert the response caps at the per-page limit for a 1-year range.",
      "preconditions": ["The audit table is seeded with > 1 page of entries."]
    }
  ],
  "out_of_scope": ["Streaming compression is not addressed by this change."]
}
```

#### `verification.acceptance_criteria`

Optional array. Each entry is a provenance-tagged acceptance criterion. The `id` is the **join key** threaded across plan → acceptance execution → evidence → triage → feedback, so it must be unique within a plan (enforced beyond the schema in `plan.semanticCheck`; a duplicate is a `*SemanticError`).

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | Slug (`^[a-z0-9][a-z0-9-]*$`), unique within `acceptance_criteria`. The plan→execution→evidence→feedback join key. |
| `statement` | yes | What must hold for the change to be accepted. |
| `source` | yes | `explicit` (stated in the ticket/spec) or `inferred` (derived by the agent). |
| `source_ref` | no | Where an explicit criterion came from (issue anchor, spec section). |
| `rationale` | only when `source = inferred` | Why the agent inferred the criterion. A schema `if/then` conditional makes it required for inferred criteria (an inferred criterion without a rationale is a `*SchemaError`). |
| `blocking` | no | Whether failing the criterion blocks acceptance. **Defaults to `true`**; downstream consumers apply the default when omitted. |
| `verify_hint` | no | A hint to the acceptance executor on how to verify. |
| `preconditions` | no | Conditions that must hold before the criterion can be verified. |
| `skip_expected` | no | Marks a criterion the acceptance agent cannot validate against the localhost preview (its trigger needs an external event the default-deny egress sandbox cannot produce). Optional boolean; omitting it leaves the criterion drivable as usual. |
| `expectation_basis` | only when `skip_expected = true` | Cites where the criterion's expectation is actually validated — e.g. the integration/e2e test with a fake. A schema `if/then` conditional makes it required when `skip_expected` is `true` (a marked criterion without a basis is a `*SchemaError`). |

When **every** criterion in `acceptance_criteria` carries `skip_expected: true` with a non-empty `expectation_basis`, the orchestrator short-circuits acceptance dispatch straight to a passed verdict (basis `all-skip-with-basis`) with no runner spawn and no preview — there is nothing the sandboxed acceptance agent could observe. A legacy criterion that omits `skip_expected` entirely never triggers the required-`expectation_basis` conditional, so older plans validate and dispatch exactly as before.

`acceptance_criteria` is annotated `x-intended-required: true` in the schema: it is additive-optional today, but a future `standard` version promotes it to required after an E31 soak period (see `AGENTS.md` → Schema change checklist). Downstream consumers are later E31 waves — **E31.5** (`plan_acceptance_precheck`) and **E31.7** (the runner acceptance agent) — which is why the field is optional now and no consumer depends on it yet.

#### `verification.out_of_scope`

Optional array of non-empty strings stating what the change deliberately does NOT cover, so reviewers and downstream acceptance don't treat an omission as a gap. Additive-optional within `standard_v1`.

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
| `scope` | `{ "files": [...] }` object | no in the schema; **required by the semantic gate when decomposing** (#1669) | The files THIS slice will touch. Same shape as the top-level `scope`. Every sub-plan must declare a non-empty `scope.files` — enforced by the plan-gate semantic validator, not the JSON Schema (see the validation rules below). |
| `depends_on` | array of integers ≥ 0 | no | 0-based indices of sibling `sub_plans` this slice depends on. Omitted/empty = no dependency (first wave). |
| `predicted_runtime_minutes` | integer ≥ 1 | yes | Estimate for this sub-plan's implement stage |
| `predicted_runtime_confidence` | `"low"` / `"medium"` / `"high"` | yes | Confidence in the sub-plan estimate |
| `model_recommendation` | `{ implement_model, rationale, complexity_assessed }` object | no | This slice's per-child model recommendation. Same shape as the top-level `model_recommendation`; resolved through the same chokepoint at the child's plan gate. |

The decomposition fan-out child minted for a sub-plan uses that sub-plan's `scope.files` — rather than the parent plan's full `scope.files` — for its implement-stage `scope_handoff` (commit bounding) and scope-drift detection, and its implement prompt binds it to implement ONLY those files (the full parent plan is shown for context only). This keeps each child bounded to its own slice instead of the whole parent change.

**Every sub-plan MUST declare its own non-empty `scope.files` when the plan decomposes (#1669).** This is a semantic-validator rule (`plan/validate.go`), described here in prose rather than encoded as a JSON-Schema `required` field — deliberately, so it needs no schema-major bump. Before #1669 a sub-plan that omitted `scope` inherited the parent's FULL `scope.files`; that made every fan-out child implement the ENTIRE plan, so the disjoint slice branches conflicted wholesale at fan-in and could never consolidate. A decomposition in which any slice omits `scope.files` is now rejected at the plan gate as a `*SemanticError` naming the offending slice(s). Each slice must own a disjoint file set — combined with the single-owner rule below, this partitions the parent scope across the slices so each fan-out child is scoped to its slice, not the whole plan.

**`depends_on` and dispatch waves (#1258).** Each sub-plan may declare `depends_on`: a list of 0-based indices into the same `sub_plans` array naming the sibling slices it must run after. The plan package derives the dispatch order with `plan.Waves`, a deterministic Kahn topological sort: wave 0 holds every slice with no unsatisfied dependency, and each subsequent wave holds the slices whose dependencies all landed in an earlier wave. Within a wave, indices are ordered ascending. A decomposition that declares no `depends_on` anywhere collapses to a single wave containing every slice (back-compat: today's parallel fan-out). A `depends_on` index that is out of range (`< 0` or `≥ len(sub_plans)`), self-referential (a slice depending on its own index), or part of a dependency cycle is rejected at the plan gate as a `*SemanticError`. No downstream dispatch consumes the waves yet — wave-ordered `run_children` dispatch lands separately (slice B, #1278).

**Runtime-sum invariant**: the validator warns (but does not reject) when the sum of `sub_plans[*].predicted_runtime_minutes` is less than the parent `predicted_runtime_minutes`. The agent may legitimately compress work when breaking it into smaller pieces; the soft warning surfaces the gap for human review.

**Lifecycle**: as of ADR-025 D4 (#455), `decomposition` is acted upon by the orchestrator. After plan approval, when the orchestrator's `Advance` would dispatch the parent's implement stage, it checks the approved plan: if `decomposition.sub_plans` is populated, the orchestrator mints one child run per sub-plan (each carrying `parent_run_id = parent.id` and `decomposed_from = parent.id`, with an issue_context built from the parent's title plus the sub-plan's `scope_hint`), parks the parent's implement stage in `awaiting_children`, and emits a `plan_decomposed` audit entry listing the child IDs. The child-completion sweeper (`backend/internal/childcompletion/`) transitions the parent stage to `succeeded` once every child reaches a terminal state successfully, or to `failed-C` if any child failed. Child runs themselves skip the fanout check (their `decomposed_from` is non-nil), so recursion is bounded at one level.

### `model_recommendation`

The agent's optional, complexity-informed recommendation for which model should execute the implement stage (#1013). Advisory — the operator ratifies or overrides it at the plan-approval gate, and the resolved model is validated against the deployment's per-adapter allowed-model set.

```json
{
  "model_recommendation": {
    "implement_model": "claude-opus-4-8",
    "rationale": "Cross-layer change threading a field through wire, domain, persistence, and render; non-trivial seams warrant the stronger model.",
    "complexity_assessed": "high"
  }
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `implement_model` | string (≥ 1 char) | yes (when the object is present) | Model identifier recommended for the implement stage (e.g. `claude-opus-4-8`). |
| `rationale` | string (≥ 1 char) | yes (when the object is present) | Why this model fits the assessed complexity. Rendered alongside the recommendation in the plan-review surface. |
| `complexity_assessed` | `"low"` / `"medium"` / `"high"` | yes (when the object is present) | The agent's complexity assessment, informing the recommendation; stamped onto calibration history. |

`model_recommendation` is one rung of the implement-model resolution ladder: deployment default < workflow-spec `executor.model` < plan `model_recommendation.implement_model` < operator gate decision. When omitted, resolution falls through to the next-lower rung (ultimately the deployment default spawn — byte-identical to today's behavior). The whole object is additive-optional within `standard_v1.x`; a plan that omits it validates unchanged. A decomposed sub-plan may carry its own `model_recommendation` (see the sub-plan table above), resolved through the same chokepoint at the child's plan gate.

### `surface_sweep_exemptions`

The plan's optional, machine-readable declarations that a surface-sweep **lockstep pattern's sibling correctly needs no change** in this plan (#1544) — the structured form of the prose "justify why a sibling needs no change" escape hatch. The plan-gate surface sweep flags a plan that scopes one member of a known multi-surface pattern (e.g. `status_template.go`) without its coupled siblings (e.g. `notifier.go`). But a path-only sweep cannot tell an `@`-mention render edit (which *does* need its `notifier.go` peer) from a system-actor render edit (which does not) — both are `status_template.go`-only. A declared exemption lets the planner assert the distinction and pass the sweep without the false-positive finding.

```json
{
  "surface_sweep_exemptions": [
    {
      "pattern": "actor @-mention render surfaces",
      "sibling": "backend/internal/issuecomment/notifier.go",
      "reason": "This adds a system-actor render (deployment_dispatched) that mentions no @-user, so the notifier.go @-mention peer needs no change."
    }
  ]
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `pattern` | string (≥ 1 char) | yes | The surface-sweep pattern's name, exactly as surfaced in the plan-gate surface-coupling sibling map (e.g. `actor @-mention render surfaces`). |
| `sibling` | string (≥ 1 char) | yes | Repo-relative path of the pattern sibling that correctly needs no change in this plan. |
| `reason` | string (≥ 1 char) | yes | Why the sibling needs no change. Rendered to plan reviewers as a **challengeable** justification, so a bogus reason is never silent. |

**When to use.** Only when you scope a lockstep pattern's trigger but a listed sibling genuinely needs no change — most commonly a purely data-driven addition to a shared render file that adds no new coupling. Do not use it to skip a sibling that actually must move in lockstep.

**How the sweep honors it.** The sweep suppresses a missing-sibling finding only when the pattern's entire missing set is covered by matching `(pattern, sibling)` exemptions; a partial exemption still fires a finding for the remaining uncovered siblings (the true positive is preserved). A declared top-level exemption also applies to every decomposition sub-plan's own scope. Each **applied** exemption is recorded in the `plan_surface_sweep` audit payload (`applied_exemptions`) and rendered into the plan-review prompt's gate evidence, so a reviewer can challenge a bogus reason — the reviewer-visibility guardrail. A non-matching, non-firing, or already-scoped-sibling exemption is a harmless no-op and is not recorded as applied. The whole array is additive-optional within `standard_v1.x`; a plan that omits it validates and sweeps exactly as before.

### `over_cap`

An optional planner **self-declaration hint** that `scope.files` exceeds the resolved implement-stage `max_files_changed` cap (#2053). The plan-stage prompt injects the resolved cap as a hard planning constraint and asks the planner to keep `scope.files` at or under it; when the work genuinely cannot fit, the planner sets `over_cap: true` as a courtesy so the flag round-trips.

```json
{
  "over_cap": true
}
```

**Advisory only — the gate signal is server-authoritative.** The plan-gate over-cap advisory is derived **server-side** from `len(scope.files)` versus the resolved cap (`runPlanWarnings`), and it fires regardless of whether `over_cap` is omitted, `false`, or `true`. No enforcement or detection path may branch on `over_cap` to decide whether a plan is over cap — the flag never suppresses (nor is required to trigger) the deterministic count-derived advisory. It exists so an honest planner can flag its own over-cap plan explicitly, not as the mechanism that surfaces the condition. Additive-optional within `standard_v1.x`; a plan that omits it validates and gates exactly as before.

### `split_proposal`

The optional ordered-phase split a plan carries when `scope.files` exceeds the resolved implement-stage `max_files_changed` cap **by count** (#2055, E50.3). It is an object with a `rationale` and an ordered `phases` array (at least two). Each phase declares its own `scope.files` (intended at or under the cap so the phase ships as its own within-cap plan), an optional `scope_hint`, and optional `depends_on` edges. The canonical shape is `expand -> migrate -> contract` for compile-atomic changes, with `depends_on` edges `expand(0) <- migrate(1) <- contract(2)`.

```json
{
  "split_proposal": {
    "rationale": "rename spans more files than the cap; split expand->migrate->contract",
    "phases": [
      { "title": "Expand",   "scope": { "files": [ { "path": "a.go", "operation": "modify" } ] } },
      { "title": "Migrate",  "depends_on": [0], "scope": { "files": [ { "path": "b.go", "operation": "modify" } ] } },
      { "title": "Contract", "depends_on": [1], "scope": { "files": [ { "path": "c.go", "operation": "modify" } ] } }
    ]
  }
}
```

**Server-authoritative, count-derived reject — regardless of `over_cap`.** A plan over the cap **by count** that carries no `split_proposal` is **REJECTED** server-side at the plan gate (a terminal `plan_review_failed` audit entry, plan stage failed category-B, no advancement). The authoritative over-cap signal is the server-derived `len(scope.files)` versus the resolved cap (`overCapSplitRejection`, reusing the same `overCapByCount` count as the advisory) — it **never reads `over_cap`**, so an over-cap-by-count monolith without a split is rejected whether `over_cap` is omitted, `false`, or `true`. An over-cap plan carrying a valid `split_proposal` is accepted; an under-cap plan is unaffected. `over_cap` remains an advisory courtesy only. See a full worked example in [`examples/split-proposal-example.yaml`](examples/split-proposal-example.yaml). Additive-optional within `standard_v1.x`; a plan that omits it validates as before.

The in-artifact `over_cap: true ⇒ split_proposal present` coupling is enforced by the plan-package semantic validator as an **additional defensive layer** (it catches a planner that self-declares `over_cap` but forgets the split), NOT the authoritative gate — the count-derived server reject above is authoritative.

## Validation rules beyond the schema

JSON Schema enforces structure. The validator (E1.5 / #20) layers on:

- `scope.files[].path` matches at least one of the stage's `allowed_paths` (when set) and none of the `forbidden_paths`.
- `generated_by.timestamp` is within the run's wall-clock window (catches clock-skew or replay).
- `generated_by.agent` matches the workflow spec's `executor.agent` for the active stage.
- `decomposition.sub_plans[*].title` must be unique within the array (semantic check in the plan package; returns `*SemanticError` on violation).
- Every sub-plan in a decomposition must declare a non-empty `scope.files`. A decomposition in which any slice omits its scope returns `*SemanticError` naming the offending slice(s) (semantic check `checkSubPlanScopesDeclared` in the plan package, #1669). An unscoped slice used to inherit the parent's full `scope.files`, which made every fan-out child implement the whole plan and produced disjoint slice branches that conflicted wholesale at fan-in and could never consolidate. This is enforced by the semantic validator, NOT the JSON Schema (no schema-major bump).
- A file path may appear in at most one sub-plan's `scope.files` within a decomposition. A path scoped by two or more slices returns `*SemanticError` (semantic check in the plan package), because the orchestrator partitions per-slice `scope.files` for commit bounding and scope-drift detection — the non-owning slice's edit to a shared file would be drift-excluded and silently shipped inert (#1062). The planner must re-slice so all edits to one file live in a single slice. (With per-slice scope now mandatory, every sub-plan is checked; the single-owner rule partitions the parent scope across disjoint slices.)
- `decomposition.sub_plans[*].depends_on` must form a valid DAG: every index in `[0, len(sub_plans))`, never self-referential, and free of cycles. `plan.Waves` validates this and returns `*SemanticError` on violation (#1258). The waves it derives are not yet consumed by dispatch (slice B, #1278).
- When `over_cap` is `true`, `split_proposal` must be present. A plan self-declaring `over_cap: true` without a `split_proposal` returns `*SemanticError` (semantic check in the plan package, #2055). This is an **additional in-artifact defensive layer**, not the authoritative over-cap gate — the authoritative enforcement is the server-side count-derived reject (`overCapSplitRejection`), which never reads `over_cap`.
- `split_proposal.phases[*].title` must be unique within the array, every phase must declare a non-empty `scope.files`, and `split_proposal.phases[*].depends_on` must form a valid DAG (every index in `[0, len(phases))`, never self-referential, free of cycles — reusing the same Kahn sort as `plan.Waves`). Each returns `*SemanticError` on violation (semantic check `checkSplitProposal` in the plan package, #2055).

These cross-references aren't expressible in JSON Schema cleanly.

### Server-side coercion

The backend applies a narrow set of coercions when an agent emits a bare string where the schema expects an object — a class of elision errors seen when agents omit wrapper keys. Coercion fires only after schema validation fails with a `*SchemaError`; `*ParseError` and semantic errors bypass it entirely.

**Covered paths and default shapes:**

| Path | Coerced to |
|---|---|
| `/ticket_reference` (bare string `s`) | `{"type": "github_issue", "url": s, "id": "unknown"}` |
| `/generated_by` (bare string `s`) | `{"agent": s, "model": "unknown", "timestamp": "<upload-time>"}` |
| `/scope/files[i]` (bare string `s`) | `{"path": s, "operation": "modify"}` |
| `/decomposition/sub_plans[i]` (bare string `s`) | `{"title": s, "scope_hint": "", "predicted_runtime_minutes": 1, "predicted_runtime_confidence": "low"}` |

After coercions are applied, the plan is re-validated against the full schema. If it passes, the coerced bytes are stored as the artifact (not the agent's original bytes) and a `plan_coerced` audit entry is appended with:

```json
{
  "run_id": "...",
  "stage_id": "...",
  "coercions": [
    {
      "field_path": "/generated_by",
      "original_type": "string",
      "original_value": "claude-code",
      "coerced_to": { "agent": "claude-code", "model": "unknown", "timestamp": "2026-05-26T12:00:00Z" }
    }
  ]
}
```

If re-validation still fails (e.g., the plan has a non-string type at a coercible location, or other schema violations persist), the upload returns 400 and the stage transitions to failed-B as normal.

**Rationale and compliance.** Coercion is a robustness mechanism, not a way to hide bad agent output. A spike in `plan_coerced` audit entries is a prompt-quality signal: the plan-stage prompt is not instructing the agent to emit the correct wrapper structure. Operators should treat rising `plan_coerced` rates as a cue to improve the prompt, not as an acceptable steady state. The coerced artifact is stored verbatim so the audit log reflects what was actually persisted.

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
