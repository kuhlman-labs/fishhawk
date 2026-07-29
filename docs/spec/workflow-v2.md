# Workflow spec v2

Reference for `.fishhawk/workflows.yaml` at major version 2. The canonical schema is [`workflow-v2.schema.json`](workflow-v2.schema.json) (JSON Schema Draft 2020-12).

> **v2 begins as a structural copy of v1 (ADR-067 / #2213; the ADR-046 major-copy precedent).** Every `$defs` entry and every non-`version` property is **validation-identical** to [`workflow-v1.schema.json`](workflow-v1.schema.json) — same keys, types, enums, `required` lists, `additionalProperties` settings, and `oneOf`/`anyOf`/`allOf` branches. Only the description strings, `$id`, `title`, and the `version` property differ. The invariant is **structural/validation equivalence, not byte identity**: descriptions are deliberately rewritten (bytes are *not* preserved), while the grammar is provably unchanged (`TestV2StructurallyMatchesV1` in `backend/internal/spec` walks both decoded trees and asserts deep equality after dropping the deliberate deltas).

## What changed from v1

- **The version enum collapses to the single token `"2"`.** Unlike v0's additive `0.3…0.7` chain and v1's `1.0…1.6` chain, **v2 has no minor chain**. Declare exactly `"2"`; the dotted minor forms (`"2.0"`, `"2.1"`, …) route to this schema by major but are **rejected** by the collapsed enum.
- **Field acceptance is by schema declaration (ADR-067 scope item 4).** A field is accepted because this schema declares it — not because the document declared a high enough minor. The inherited minor-gating prose (`Requires version 0.N+`, `accepted at every advertised version`) has been rewritten to field-presence language throughout the v2 descriptions.

## Grammar

The grammar is **identical to v1 this slice** — every stage type, executor branch, constraint kind, produces artifact, gate shape, delegation block, and reviewer shape carries over unchanged. For the full reference see [`workflow-v1.md`](workflow-v1.md) (the v1 deploy/acceptance/egress/agent-version/verification/diff-coverage surfaces) and [`workflow-v0.md`](workflow-v0.md) (the base grammar). A minimal v2 spec differs from a v1 spec only in its `version` value:

```yaml
version: "2" # required; routes to workflow-v2.schema.json (no minor form — "2.0" is rejected)
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
```

Sibling E52 children grow this page as they change the v2 grammar; until then the v1 reference is authoritative for every member.

## Version routing

The backend (`backend/internal/spec`) and the CLI (`cli/internal/spec`) compile the workflow-v0, workflow-v1, **and** workflow-v2 schemas at init and dispatch a spec to one of them by its `version` **major** component:

- `version: "0.x"` → `workflow-v0.schema.json`
- `version: "1.x"` → `workflow-v1.schema.json`
- `version: "2"` → `workflow-v2.schema.json` (a dotted `"2.0"` routes here by major but the collapsed enum rejects it)
- a missing / non-string / unparseable `version` falls through to the v0 schema, which then emits the existing required-version error (so a malformed version never silently passes)
- a well-formed but unrecognized major (`>= 3`) **fails closed** with an error naming the supported majors (`0, 1, 2`)

`/healthz` advertises the `workflow-v0`, `workflow-v1`, and `workflow-v2` embedded-schema hashes so a component can detect drift in any of them.

## See also

- [`workflow-v1.md`](workflow-v1.md) — the full grammar v2 currently copies.
- [`workflow-v0.md`](workflow-v0.md) — the base grammar.
- [`README.md`](README.md) — the versioning + coexistence policy.
- `docs/ARCHITECTURE.md` §10 — workflow spec grammar and version routing.
