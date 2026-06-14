# Clarification request artifact (standard_v1 sibling)

Canonical schema: [`clarification-request-v1.schema.json`](clarification-request-v1.schema.json).
Embedded copies: `backend/internal/plan/schemas/`, `runner/internal/plan/schemas/` (kept in lockstep by `scripts/sync-schemas`).

## What it is

The plan stage normally produces a [`standard_v1` plan](plan-standard-v1.md). When the planner's step-zero plannability / needs-direction check determines an issue is **not yet plannable** — it lacks a non-derivable fact, or it needs an operator policy decision (the ADR-040 FACTS / DECISIONS bucket test) — the planner instead emits a `clarification_request` artifact and the stage parks at the `awaiting_input` gate rather than guessing a plan.

The artifact is an **additive sibling** of the plan artifact, not a new plan version. It is selected by the top-level `kind` discriminator **before** validation, so [`plan-standard-v1.schema.json`](plan-standard-v1.schema.json) stays frozen (`additionalProperties: false`):

- `kind == "clarification_request"` → validate against `clarification-request-v1`.
- otherwise (the artifact carries `plan_version`) → validate against `plan-standard-v1`.

The clarification artifact intentionally omits `plan_version` and the plan artifact has no `kind`, so the two never collide on a shared required field.

## Discrimination and validation

`backend/internal/plan` and `runner/internal/plan` expose the routing:

- `DetectArtifactKind(data)` — peeks at the top-level `kind` and returns `ArtifactKindClarificationRequest` or `ArtifactKindPlan` (the default).
- `ValidateArtifact(data)` — discriminates, then validates against the matching schema.
- `ValidateClarificationRequest(data)` — schema-validates **and** enforces that question `id`s are unique (see below).
- `ParseClarificationRequest(data)` (backend only) — `ValidateClarificationRequest` plus a typed decode into `*ClarificationRequest`.

`Validate` (plan-only) is unchanged, so a `standard_v1` plan still validates exactly as before.

### Unique question ids

Operator answers are keyed by question `id` on resume, so a duplicate id is ambiguous. JSON Schema Draft 2020-12 cannot express cross-item uniqueness on an object sub-field, so the **validate path** in both the runner and the backend enforces it semantically — not only the typed parse path. A `clarification_request` carrying two questions with the same `id` is rejected.

## Fields

| Field | Required | Notes |
|---|---|---|
| `kind` | yes | `const: "clarification_request"` — the discriminator. |
| `ticket_reference` | yes | Same shape as the plan artifact. |
| `generated_by` | yes | Same shape as the plan artifact. |
| `summary` | yes | Why the issue is not yet plannable; the lead paragraph of the `awaiting_input` ping comment. |
| `questions` | yes | ≥ 1 parked questions; `id`s must be unique. |

Each question:

| Field | Required | Notes |
|---|---|---|
| `id` | yes | Stable, unique key (`^[a-z0-9][a-z0-9_-]*$`, ≤ 64). Operator answers are matched back by this. |
| `question` | yes | The decision or missing fact, as a direct question. |
| `what_i_can_infer` | no | What the agent already established, narrowing the question to the genuinely non-derivable part. |
| `recommended_default` | yes | The option the agent would take absent an answer. The **calibration guard** makes this required: parking without a default is not allowed. |
| `tradeoffs` | yes | Consequences of the default versus the alternatives. |

## Example

```json
{
  "kind": "clarification_request",
  "ticket_reference": {
    "type": "github_issue",
    "url": "https://github.com/kuhlman-labs/fishhawk/issues/1057",
    "id": "kuhlman-labs/fishhawk#1057"
  },
  "generated_by": {
    "agent": "claude-code",
    "model": "claude-opus-4-8",
    "timestamp": "2026-06-13T21:00:00Z"
  },
  "summary": "The issue asks to add rate limiting but does not say which backend store or limit policy to use; both are operator decisions, not derivable from the codebase.",
  "questions": [
    {
      "id": "rate-limit-store",
      "question": "Which store should back the rate limiter — in-process memory or Redis?",
      "what_i_can_infer": "The repo has no Redis client wired today; an in-process limiter is the smaller change.",
      "recommended_default": "In-process token bucket, since no shared store exists yet.",
      "tradeoffs": "In-process resets on restart and does not coordinate across replicas; Redis adds a dependency but survives restarts and is multi-replica correct."
    },
    {
      "id": "limit-policy",
      "question": "What request/minute ceiling and burst should the default policy use?",
      "recommended_default": "60 req/min with a burst of 10.",
      "tradeoffs": "Tighter limits protect the backend but may reject legitimate bursts; looser limits invert that."
    }
  ]
}
```

## Validating locally

```sh
check-jsonschema --check-metaschema docs/spec/clarification-request-v1.schema.json
# validate an artifact against the schema:
check-jsonschema --schemafile docs/spec/clarification-request-v1.schema.json path/to/artifact.json
```
