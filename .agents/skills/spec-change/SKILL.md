---
name: spec-change
description: Change a Fishhawk canonical schema or API contract end to end — `docs/spec/*.schema.json` (workflow, plan, operator overlay), the embedded mirrors, the generated site reference, version advertising, and `docs/api/v0.openapi.yaml`. Use when adding/changing a field in the workflow spec or plan schema, standing up a new workflow major, or changing the REST API surface.
---

# Change a schema or API contract

Canonical sources live in `docs/spec/` (schemas + reference docs) and `docs/api/` (OpenAPI). Each is mirrored or rendered into several places, and CI fails on every one that's missed.

## 1. Classify the change first

- **Additive.** A new *optional* field within the current major. Proceed.
- **Breaking.** A new *required* field, a removed field, or a narrowed type, in an existing major. Don't do it in place: bump the major (`workflow-vN+1`, `standard_v2`).
  - For a field meant to become required later, add it optional and annotate it with `"x-intended-required": true`.
  - Declare the soak window in the PR body under `## Notes`.

State the classification to the user before editing.

## 2. Edit and propagate (workflow / plan schemas)

1. Edit the canonical `docs/spec/<name>.schema.json` **and** its reference doc `docs/spec/<name>.md`. For the workflow spec, `docs/spec/workflow-v2.md` is the complete reference for the live major.
2. `scripts/sync-schemas` mirrors the change into `backend/internal/spec/schemas/` and `cli/internal/spec/schemas/` (and the plan mirrors). Commit the mirrors with it.
3. `scripts/gen-site-reference` re-renders the generated regions of `site/src/content/docs/reference/*.md`. Edit only outside the `BEGIN/END GENERATED` markers.
4. Validate the examples:
   - `check-jsonschema --schemafile docs/spec/<schema> <example.yaml>`
   - A workflow-v2 doc using `defaults`/`extends` must go through the product validator:
     `fishhawk validate --emit-resolved <yaml> | check-jsonschema --schemafile docs/spec/workflow-v2.schema.json --default-filetype yaml -`
     Treat `fishhawk validate` (without the flag) as the authority.
5. **Version advertising.** For a new version string:
   - add it to the plan validator's recognized set (`backend/internal/plan/`)
   - add it to the runner `/healthz` schema-versions list

### A NEW workflow major (also)

- Add `docs/spec/workflow-vN.schema.json`, then run `scripts/sync-schemas`. The `workflow-v*` glob routes it; no script edit is needed.
- Append `{Major: N, Path: …}` to `embeddedSchemas` in **both** `backend/internal/spec/parse.go` and `cli/internal/spec/spec.go`. Without it, the validator fails closed on the new major.
- Add `EmbeddedSchemaHashVN()` plus the `workflow-vN` entry in the `/healthz` `schemas` map (`backend/internal/server/handlers.go`).
- Register the mirror set as a surface-sweep pattern in `backend/internal/server/surface_sweep.go`.
- Bumping `.fishhawk/workflows.yaml`'s `version:` major makes `scripts/dev up/reload` print the schema-major MCP banner. Expect it.

## 3. REST API changes

- **Source of truth:** `docs/api/v0.openapi.yaml`. Update the human companion `docs/api/v0.md` with it.
- **Lint with the pinned version:** `npx -y @redocly/cli@2.31.5 lint --config docs/api/redocly.yaml docs/api/v0.openapi.yaml`
- Re-run `scripts/gen-site-reference`; `reference/api.md` is generated from it.
- A tightened auth check also needs the PR-body Auth change checklist (`fishhawkd token migrate` dry-run inventory). See `AGENTS.md`.

## 4. Gate

Run the `verify-gate` skill. Its schema-sync step catches a missed mirror, and the site-reference drift test catches a missed regeneration.
