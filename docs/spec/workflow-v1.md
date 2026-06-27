# Workflow spec v1

Reference for `.fishhawk/workflows.yaml` at major version 1. The canonical schema is [`workflow-v1.schema.json`](workflow-v1.schema.json) (JSON Schema Draft 2020-12).

> **v1 begins as a structural copy of v0 (ADR-046 / #1381).** The `$defs` and `properties` are byte-for-byte identical to [`workflow-v0.schema.json`](workflow-v0.schema.json); the only differences are `$id`, `title`, and the top-level `version` enum (`["1.0"]`). No v1-specific grammar has been added yet — **deploy content is deferred to E23.2.** This issue (#1381) stands up the versioning *mechanism* — a separate schema file and a version-routed validator — so the v1 deploy fields can land additively later without touching v0.

## Grammar

Identical to v0. For the full field reference (top-level shape, stages, executors, inputs, produces, constraints, budgets, gates, operator-agent delegation, decomposition controls), see [`workflow-v0.md`](workflow-v0.md). A v1 spec differs from a v0 spec only in its `version` value:

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
