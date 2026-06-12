# Fishhawk specs

Machine-readable schemas and reference docs for the v0 surfaces that span the runner, the backend, and customer repos. The workflow and plan schemas freeze at Day 21 of the v0 build (`MVP_SPEC.md` §8) — never break in place; bump to a new version (`workflow-v1`, `standard_v2`, `operator-role-v1`...) and keep the old schema readable.

## Files

| Spec | Reference doc | JSON Schema | Example(s) |
|---|---|---|---|
| Workflow spec v0 (`.fishhawk/workflows.yaml`) | [`workflow-v0.md`](workflow-v0.md) | [`workflow-v0.schema.json`](workflow-v0.schema.json) | [`examples/workflow-v0-feature-change.yaml`](examples/workflow-v0-feature-change.yaml), [`examples/workflow-v0-routine-change.yaml`](examples/workflow-v0-routine-change.yaml) |
| Plan artifact `standard_v1` | [`plan-standard-v1.md`](plan-standard-v1.md) | [`plan-standard-v1.schema.json`](plan-standard-v1.schema.json) | [`examples/plan-standard-v1-example.json`](examples/plan-standard-v1-example.json) |
| Operator role spec v0 (shipped default + `.fishhawk/operator.yaml` overlay, ADR-040) | [`operator-role.md`](operator-role.md) | [`operator-role.schema.json`](operator-role.schema.json), [`operator-role-overlay.schema.json`](operator-role-overlay.schema.json) | [`operator-role-default.yaml`](operator-role-default.yaml) (shipped default — a product artifact, synced like the schemas), [`examples/operator-role-overlay-example.yaml`](examples/operator-role-overlay-example.yaml) |

All schemas are JSON Schema Draft 2020-12.

## Validating locally

```sh
# install once
brew install check-jsonschema

# validate the schemas themselves against the JSON Schema meta-schema
check-jsonschema --check-metaschema docs/spec/*.schema.json

# validate examples (and the live placeholder workflow file)
check-jsonschema --schemafile docs/spec/workflow-v0.schema.json \
    docs/spec/examples/workflow-v0-feature-change.yaml \
    docs/spec/examples/workflow-v0-routine-change.yaml \
    .fishhawk/workflows.yaml

check-jsonschema --schemafile docs/spec/plan-standard-v1.schema.json \
    docs/spec/examples/plan-standard-v1-example.json

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

Additive, non-breaking changes within a major version are permitted (e.g., adding an optional field) — but these are still rare and require a corresponding update to the validator's tests.

## See also

- `docs/MVP_SPEC.md` §4.1–§4.3 — the primitives, the canonical example, and the plan-artifact requirements.
- `docs/ARCHITECTURE.md` §4 — workflow run lifecycle, where these artifacts flow.
- `CLAUDE.md` — the canonical-references list that points future agents here.
