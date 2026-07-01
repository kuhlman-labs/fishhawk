# Workflow preset library (ADR-048 / E29.1)

The preset library is the seed for onboarding: `fishhawk init` (E29.3)
and the App-PR path (E29.7) turn a chosen tier plus a few structured
deltas into a schema-valid `.fishhawk/workflows.yaml`. Three canonical
presets ship, one per autonomy tier in `docs/METHODOLOGY.md`.

## The three presets

Each preset is a complete, schema-valid `workflow-v1` document
(`version: "1.0"`, a `roles.founder` role, a single `feature_change`
workflow). They differ ONLY in the `operator_agent` delegation block —
the stages, reviewers, budgets, constraints, and gates are identical.

| Preset | METHODOLOGY tier | `operator_agent` delegation |
|---|---|---|
| `workflow-preset-low.yaml` | Low (human-led) | No `operator_agent` block. Fail-closed: every judgment point (approve, fixup, retry, waive, merge) pages the human. |
| `workflow-preset-medium.yaml` | Medium (default) | `may_approve: clean_dual_approval`, `may_route_fixup: convergent_concerns`, `may_retry: infra_flake` + the 7-event `must_page_human` list. Waive and merge stay human. Byte-for-byte the current `.fishhawk/workflows.yaml` `feature_change` block. |
| `workflow-preset-high.yaml` | High (agent merges) | Medium's three knobs plus `may_waive: solo_low` and `may_merge: gates_resolved_ci_green`. |

`medium` is authoritative: `low` is `medium` with the `operator_agent`
block removed, `high` is `medium` with the two extra knobs added. The
`operator_agent` def in `docs/spec/workflow-v1.schema.json` declares no
required knobs (`$defs.operator_agent` has no `required` array), so the
low preset omitting the block and the high preset adding two knobs both
validate.

## The generator and its delta surface

`cli/internal/spec/preset.go` implements `Generate(preset, deltas)`: it
loads the chosen preset's canonical bytes, applies structured deltas via
`yaml.v3` node edits (preserving comments and ordering — no struct
round-trip), then validates the result through the existing
`ValidateBytes` gate before returning. A delta that breaks schema
validity fails closed rather than emitting an invalid document.

The delta surface (`spec.Deltas`):

| Delta | Effect |
|---|---|
| Budget ceiling | Overrides `budgets[0].limit_usd` (the weekly advisory cost limit). |
| Single vs dual reviewers | Drops the Codex (`gpt-5.5`) reviewer from every stage's `reviewers.agents`, leaving Claude only. |
| Human gates | Selects which of the plan / review approval gates remain human-approved (a gate not selected is left as authored). |

The generator lives in `cli/internal/spec` so `fishhawk init` stays
standalone — no backend round-trip. The backend/CLI module wall
(`backend/` and `cli/` cannot import each other's `internal/` packages)
means the generator cannot be shared; the backend side (E29.7 App-PR)
receives the mirrored embedded presets plus a validation test now, and
its own generator is deferred to E29.7.

## Embed / mirror / sync discipline

The canonical presets live under `docs/spec/`. They are mirrored into
both module sides so each embeds its own copy (no cross-module import):

- `cli/internal/spec/presets/workflow-preset-*.yaml`
- `backend/internal/spec/presets/workflow-preset-*.yaml`

`scripts/sync-schemas` copies the canonical `docs/spec/workflow-preset-*.yaml`
into both mirror directories — run it after editing any preset and
commit the mirrors so the `//go:embed` directives resolve. The CI
schema-sync gate red-lines a drifted mirror, exactly as it does for the
JSON schemas and `operator-role-default.yaml`.

Drift-proof tests validate every mirrored preset against the embedded
`workflow-v1` schema on BOTH sides: `cli/internal/spec/preset_test.go`
via `ValidateBytes`, `backend/internal/spec/preset_test.go` via
`ParseBytes` + `Validate` (schema + semantic). The same bytes validated
through both embed copies is the cross-boundary check that the
canonical, the CLI mirror, and the backend mirror stay in lockstep.
