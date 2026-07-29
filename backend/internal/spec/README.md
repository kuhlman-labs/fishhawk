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

**workflow-v2 (ADR-067 / #2213)** BEGAN as a structural copy of v1: at the copy every `$defs` entry and non-`version` property was validation-identical (same keys, types, enums, `required`, `additionalProperties`, and `oneOf`/`anyOf`/`allOf` branches), with only the descriptions, `$id`, `title`, and the `version` property differing. Its enum is the single token `"2"` (no minor chain; the dotted `"2.0"` form routes here by major but the enum rejects it). The E52 children now diverge it, so the copy-fidelity check is an ALLOW-LIST diff rather than deep equality: `TestV2DivergesFromV1OnlyByLicensedDeltas` (the re-scoped `TestV2StructurallyMatchesV1`, per the #2320 disposition) walks both decoded mirrors and asserts the divergence set equals EXACTLY the declared `licensedV2Deltas` table. The assertion is two-directional — an unlisted divergence fails (the accidental-drop net #2213 bought) and a listed path that no longer diverges fails (a stale entry cannot rot) — and its normalizer is position-aware, so a `description` key is stripped only as a schema annotation, never as a property NAME inside a `properties` / `$defs` map. Guardrail: past roughly 15 licensed entries the allow-list stops being a readable statement of intent, and #2320 should be revisited to retire the test rather than keep appending.

## workflow-v2 removed back-compat forms (E52.3 / #2215)

`v2removed.go` holds the version-gated raw-document sweep for the two back-compat surfaces v2 deletes.

| Removed in v2 | Replacement |
| --- | --- |
| `operator_agent.must_page_human: [reviewer_reject]` | `gating_reviewer_reject` / `advisory_reviewer_reject` |
| `reviewers.agent: <N>` | `reviewers.agents[]` (effective count = `len(agents)`) |

- **Why a sweep at all.** The schema already rejects both — the bare token falls out of the `must_page_human` enum, and `agent` becomes an undeclared property of an `additionalProperties: false` block — but neither generic message names the replacement. `checkV2RemovedForms` returns a `*SchemaError` that does.
- **Ordering.** `ParseBytes` runs the sweep AFTER version routing and BEFORE `schema.Validate`, so the actionable message wins over the generic one. Asserted by the message-content tests in `v2removed_test.go`, not assumed.
- **Version gate.** The sweep runs only for a routed major `>= 2`. `schemaForVersion` returns the routed major alongside the schema; the fall-through-to-v0 branches (missing / non-string / unparseable `version`) return major 0, so those paths keep their pre-existing required-version error verbatim, and an unsupported major fails closed at routing before the sweep.
- **Matching contract.** The walk is GENERIC — it matches by key name at any depth, reaching the workflow-level `operator_agent`, any per-gate `operator_agent`, and any stage's `reviewers` without encoding their positions. It deliberately OVER-TRIGGERS in exchange for never missing a legacy form: a legacy form in a position v2 does not permit is still reported with the removed-form message, so in an already-invalid document the legacy-form message may precede the genuine structural error. That trade is intentional (a position-aware sweep would re-encode schema structure E52 is actively restructuring) and is pinned by `TestCheckV2RemovedForms_MatchesByKeyNameNotPosition`. Map keys are walked in sorted order so the reported first match is deterministic.
- **CLI mirror.** `cli/internal/spec/validate.go` carries a byte-identical pair of messages and the same walk, wired into `ValidateBytes` under the same major gate and ordering — `fishhawk validate` is where a spec author most often meets these errors. The modules are deliberately separate, so both copies carry message-content assertions rather than sharing a constant.
- **Retained Go symbols.** No type, constant, or method is deleted. `PageEventReviewerReject`, `ReviewersConfig.Agent`, `AgentCount()`'s bare-count fallback, and `delegation.reviewerRejectClass` all stay — v0 and v1 documents still parse into the shared types. A v2 document simply cannot reach them, which is what satisfies the done-means structurally.

## Agent-version compatibility ranges (v1.4, E32.13 / #1743)

`executor.agent_version` + `reviewers.agents[i].agent_version` declare the semver comparator RANGE (e.g. `">=2.1 <2.2"`) of agent CLI versions a workflow was validated against, failing dispatch loudly when the resolved CLI version falls outside it (the #1741 opaque-CLI-drift diagnosis).

- Matcher/validator: `spec.ValidAgentVersionRange` / `spec.MatchAgentVersionRange` in `backend/internal/spec/agentversion.go`, with a byte-parity twin in `cli/internal/spec/agentversion.go`. Called by the semantic validator (`validate.go`).
- The executor range is threaded to the runner via `promptResponse.agent_version_range` (`backend/internal/server/prompt.go`).
- The runner's own duplicated `matchAgentVersionRange` (`runner/cmd/fishhawk-runner/main.go`) fails the stage **pre-spawn category-C** (`agent_version_mismatch`) on an out-of-range resolved (#1769-probed) version, or degrades-and-proceeds (`agent_version_uncomparable`) on an unprobeable one.
- Absent range = no constraint.
- The binary pin stays a host concern via `FISHHAWK_AGENT_BIN` / `FISHHAWK_CODEX_BIN` (#1741 / #1769).
- The reviewer-side (codex-only) enforcement is a sibling change.
