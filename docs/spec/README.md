# Fishhawk specs

Machine-readable schemas and reference docs for the v0 surfaces that span the runner, the backend, and customer repos. The workflow and plan schemas freeze at Day 21 of the v0 build (`MVP_SPEC.md` §8) — never break in place; bump to a new version (`workflow-v1`, `standard_v2`, `operator-role-v1`...) and keep the old schema readable.

## Files

| Spec | Reference doc | JSON Schema | Example(s) |
|---|---|---|---|
| Workflow spec v0 (`.fishhawk/workflows.yaml`) | [`workflow-v0.md`](workflow-v0.md) | [`workflow-v0.schema.json`](workflow-v0.schema.json) | [`examples/workflow-v0-feature-change.yaml`](examples/workflow-v0-feature-change.yaml), [`examples/workflow-v0-routine-change.yaml`](examples/workflow-v0-routine-change.yaml) |
| Workflow spec v1 (`.fishhawk/workflows.yaml`, ADR-046) | [`workflow-v1.md`](workflow-v1.md) | [`workflow-v1.schema.json`](workflow-v1.schema.json) | [`examples/workflow-v1-acceptance.yaml`](examples/workflow-v1-acceptance.yaml) (feature_change with a v1.3 acceptance stage — the verbatim operator companion-commit stanza); base grammar in [`workflow-v0.md`](workflow-v0.md) |
| Workflow spec v2 (`.fishhawk/workflows.yaml`, ADR-067) | [`workflow-v2.md`](workflow-v2.md) — the **complete standalone** reference for major 2 (E52.9 / #2221); no earlier major is needed to look up a v2 field | [`workflow-v2.schema.json`](workflow-v2.schema.json) | [`examples/workflow-v2-backlog-grooming.yaml`](examples/workflow-v2-backlog-grooming.yaml) (the SHIPPED backlog-grooming declaration: a NON-code-change workflow on the plan/implement/review types — propose → gate → apply, producing no diff (E52.7 / #2219), carrying the non-diff `applies_to: {trigger: […]}` routing form, `autonomy: low` plus the four-class grooming action matrix, and a forge-neutral `approvals` gate per gated stage (E54.4 / #2236); read from disk and asserted by `TestShippedGroomingExample_*`), [`examples/workflow-v2-reuse.yaml`](examples/workflow-v2-reuse.yaml) + its generated resolved form [`examples/workflow-v2-reuse.resolved.json`](examples/workflow-v2-reuse.resolved.json) (`defaults` / `extends`; E52.4 / #2216) |
| workflow-v1 → workflow-v2 migration (`fishhawk migrate-spec`, E52.8 / #2220) | [`workflow-migration.md`](workflow-migration.md) | — (translates between the v1 and v2 schemas above) | golden fixture pair in `cli/internal/spec/testdata/migrate/` |
| Plan artifact `standard_v1` | [`plan-standard-v1.md`](plan-standard-v1.md) | [`plan-standard-v1.schema.json`](plan-standard-v1.schema.json) | [`examples/plan-standard-v1-example.json`](examples/plan-standard-v1-example.json) |
| Clarification request artifact (`standard_v1` sibling) | [`clarification-request-v1.md`](clarification-request-v1.md) | [`clarification-request-v1.schema.json`](clarification-request-v1.schema.json) | inline in [`clarification-request-v1.md`](clarification-request-v1.md#example) |
| Grooming report artifact (plan-stage sibling, ADR-065 §3) | [`grooming-report-v1.md`](grooming-report-v1.md) | [`grooming-report-v1.schema.json`](grooming-report-v1.schema.json) | [`examples/grooming-report-v1-example.json`](examples/grooming-report-v1-example.json) |
| Operator role spec v0 (shipped default + `.fishhawk/operator.yaml` overlay, ADR-040) | [`operator-role.md`](operator-role.md) | [`operator-role.schema.json`](operator-role.schema.json), [`operator-role-overlay.schema.json`](operator-role-overlay.schema.json) | [`operator-role-default.yaml`](operator-role-default.yaml) (shipped default — a product artifact, synced like the schemas), [`examples/operator-role-overlay-example.yaml`](examples/operator-role-overlay-example.yaml) |

All schemas are JSON Schema Draft 2020-12.

## Validating locally

```sh
# install once
brew install check-jsonschema

# validate the schemas themselves against the JSON Schema meta-schema
check-jsonschema --check-metaschema docs/spec/*.schema.json

# validate examples. A workflow file is validated against the major it
# DECLARES: this repo's own .fishhawk/workflows.yaml declares version "1.3", so
# it belongs in the v1 command below and moves to the v2 command once the
# operator migration to workflow-v2 lands (see workflow-migration.md).
check-jsonschema --schemafile docs/spec/workflow-v0.schema.json \
    docs/spec/examples/workflow-v0-feature-change.yaml \
    docs/spec/examples/workflow-v0-routine-change.yaml

check-jsonschema --schemafile docs/spec/workflow-v1.schema.json \
    docs/spec/examples/workflow-v1-acceptance.yaml \
    .fishhawk/workflows.yaml

# a v2 spec declares version: "2" — the dotted minor form is rejected.
check-jsonschema --schemafile docs/spec/workflow-v2.schema.json \
    docs/spec/examples/workflow-v2-backlog-grooming.yaml

# NOT examples/workflow-v2-reuse.yaml: v2 `defaults` / `extends` are resolved
# BEFORE schema validation, so the schema only ever sees the RESOLVED document
# (workflow-v2.md § "Where errors point"). A raw check-jsonschema run rejects
# the unresolved source — a stage inheriting its executor reads as missing one.
# Use `fishhawk validate` (which resolves reuse first) for a reuse-bearing spec.

check-jsonschema --schemafile docs/spec/plan-standard-v1.schema.json \
    docs/spec/examples/plan-standard-v1-example.json

check-jsonschema --schemafile docs/spec/grooming-report-v1.schema.json \
    docs/spec/examples/grooming-report-v1-example.json

check-jsonschema --schemafile docs/spec/operator-role.schema.json \
    docs/spec/operator-role-default.yaml

check-jsonschema --schemafile docs/spec/operator-role-overlay.schema.json \
    docs/spec/examples/operator-role-overlay-example.yaml
```

The Go-based validators that ship in the runner and backend (E1.3 / #18, E1.5 / #20) are the canonical enforcement point; this CLI is for local sanity-checks and review.

## Versioning

- The schema's filename pins the version (`workflow-v0.schema.json`, `plan-standard-v1.schema.json`).
- Inside each schema, the `$id` URL also pins the version.
- `version` (workflow spec), `plan_version` (plan artifact), and `spec_version` (operator role) are required, single-value enums.
- Breaking changes go to a new file (`workflow-v1.schema.json`, `plan-standard-v2.schema.json`). The validators carry every version forever so old audit log entries stay readable.
- **Coexistence is live for the workflow spec across three majors (ADR-046 / #1381; ADR-067 / #2213).** `workflow-v0.schema.json`, `workflow-v1.schema.json`, and `workflow-v2.schema.json` coexist; the backend and CLI validators compile all three at init and route a spec by its `version` major (`0.x` → v0, `1.x` → v1, `2.x` → v2), failing closed on an unrecognized major (`≥ 3`). v1 began as a structural copy of v0 and then added the deploy/acceptance surfaces; **v2 BEGAN as a structural copy of v1** — validation-identical grammar, only descriptions, `$id`, `title`, and the `version` property differing — with the version enum collapsed to the single token `"2"` (no minor chain; field acceptance is by schema declaration, ADR-067 scope item 4); the E52 children have since diverged it into a schema of its own, so the interim copy-fidelity check is **retired** (#2320) and each deliberate delta is recorded in the appendix of [`workflow-v2.md`](workflow-v2.md) and in `backend/internal/spec/README.md`. Since E52.9 (#2221), [`workflow-v2.md`](workflow-v2.md) is a **complete standalone reference** — v0 and v1 are no longer the reader's path to a v2 field. `/healthz` advertises the `workflow-v0`, `workflow-v1`, and `workflow-v2` embedded-schema hashes.
- **v0 and v1 are FROZEN majors, not removed, and no removal is planned.** Both are pinned by content digest in `backend/internal/spec` (`TestFrozenMajorsV0AndV1AreImmutable`) so an edit to a shipped major fails a test rather than only a convention. Their schemas stay embedded, compiled, routed and validated forever: every audit-log run and every already-committed spec keeps working with no action. A **new** spec on v0 or v1 is still **accepted** — nothing rejects a `0.x` / `1.x` document — but is **discouraged**: `fishhawk init` emits v2 presets, the reuse / legibility / autonomy work exists only at v2, and [`workflow-migration.md`](workflow-migration.md) (`fishhawk migrate-spec`) translates an existing v1 document with an approval-eligibility report. No removal date is set.

Additive, non-breaking changes within a major version are permitted (e.g., adding an optional field) — but these are still rare and require a corresponding update to the validator's tests.

## See also

- `docs/MVP_SPEC.md` §4.1–§4.3 — the primitives, the canonical example, and the plan-artifact requirements.
- `docs/ARCHITECTURE.md` §4 — workflow run lifecycle, where these artifacts flow.
- `CLAUDE.md` — the canonical-references list that points future agents here.
