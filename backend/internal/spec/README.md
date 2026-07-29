# backend/internal/spec

Workflow-spec parsing, semantic validation, and version-routed embedded JSON Schemas for `.fishhawk/workflows.yaml`.

## Version routing (ADR-046 / ADR-067)

`parse.go` embeds the `workflow-v0`, `workflow-v1`, and `workflow-v2` schemas and compiles each at package init. `ParseBytes` reads the spec's `version`, parses its **major** component, and validates against the schema for that major via the `embeddedSchemas` routing table:

- `0.x` → `workflow-v0.schema.json`
- `1.x` → `workflow-v1.schema.json`
- `2.x` → `workflow-v2.schema.json`
- missing / non-string / unparseable `version` → falls through to v0 (preserving the existing required-version error, so a malformed version never silently passes)
- well-formed but unrecognized major (`≥ 3`) → **fails closed** with a `*SchemaError` naming the supported majors (derived from the table)

Adding a major means embedding its schema, appending a `{Major, Path}` row to `embeddedSchemas`, and adding an `EmbeddedSchemaHashVN()` accessor + `/healthz` `schemas` entry — everything else (`computeSupportedMajors`, `computeSchemaHashes`, `schemaForVersion`) derives from the table. See the "A NEW workflow MAJOR" checklist in `AGENTS.md`.

**workflow-v2 (ADR-067 / #2213)** is a STRUCTURAL copy of v1: every `$defs` entry and non-`version` property is validation-identical (same keys, types, enums, `required`, `additionalProperties`, and `oneOf`/`anyOf`/`allOf` branches) — only the descriptions, `$id`, `title`, and the `version` property differ. Its enum is the single token `"2"` (no minor chain; the dotted `"2.0"` form routes here by major but the enum rejects it). `TestV2StructurallyMatchesV1` walks both decoded mirrors and asserts deep equality after normalizing away those deliberate deltas.

## Agent-version compatibility ranges (v1.4, E32.13 / #1743)

`executor.agent_version` + `reviewers.agents[i].agent_version` declare the semver comparator RANGE (e.g. `">=2.1 <2.2"`) of agent CLI versions a workflow was validated against, failing dispatch loudly when the resolved CLI version falls outside it (the #1741 opaque-CLI-drift diagnosis).

- Matcher/validator: `spec.ValidAgentVersionRange` / `spec.MatchAgentVersionRange` in `backend/internal/spec/agentversion.go`, with a byte-parity twin in `cli/internal/spec/agentversion.go`. Called by the semantic validator (`validate.go`).
- The executor range is threaded to the runner via `promptResponse.agent_version_range` (`backend/internal/server/prompt.go`).
- The runner's own duplicated `matchAgentVersionRange` (`runner/cmd/fishhawk-runner/main.go`) fails the stage **pre-spawn category-C** (`agent_version_mismatch`) on an out-of-range resolved (#1769-probed) version, or degrades-and-proceeds (`agent_version_uncomparable`) on an unprobeable one.
- Absent range = no constraint.
- The binary pin stays a host concern via `FISHHAWK_AGENT_BIN` / `FISHHAWK_CODEX_BIN` (#1741 / #1769).
- The reviewer-side (codex-only) enforcement is a sibling change.
