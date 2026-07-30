# Workflow preset library (ADR-048 / E29.1)

The preset library is the seed for onboarding: `fishhawk init` (E29.3)
and the App-PR path (E29.7) turn a chosen tier plus a few structured
deltas into a schema-valid `.fishhawk/workflows.yaml`. Three canonical
presets ship, one per autonomy tier in `docs/METHODOLOGY.md`.

Since E52.8 (#2220) they are **workflow-v2** documents (`version: "2"`).

## The three presets

Each preset is a complete, schema-valid `workflow-v2` document with a
single `feature_change` workflow. They are **one shared base plus a
single `autonomy:` line**: the only difference between the three files,
beyond the leading header comment block, is that one word.

| Preset | METHODOLOGY tier | Tier line | Delegation the tier expands to |
|---|---|---|---|
| `workflow-preset-low.yaml` | Low (human-led) | `autonomy: low` | Every action class at `mode: gated`. Nothing delegated; every judgment point (approve, fixup, retry, waive, merge) pages the human. No `page_human_on` list is implied — the tier absorbs nothing, so it excepts nothing. |
| `workflow-preset-medium.yaml` | Medium (default) | `autonomy: medium` | `approve` (clean_dual_approval), `fixup` (convergent_concerns) and `retry` (infra_flake) at `mode: auto`; `waive` and `merge` gated. Plus the seven-event page list. |
| `workflow-preset-high.yaml` | High (agent merges) | `autonomy: high` | Medium plus `waive` (solo_low) and `merge` (gates_resolved_ci_green) at `mode: auto`. Same seven-event page list. |

The seven-event page list both medium and high expand to:
`gating_reviewer_reject`, `plan_rejection`, `scope_amendment`,
`budget_override`, `policy_override`, `exception_request`,
`requirement_arbitration`. The v0/v1 bare `reviewer_reject` token is not
declarable at major 2 (E52.3 / #2215) — `gating_reviewer_reject` is the
sense it always resolved to.

`autonomy:` is a *shorthand*. An explicit `actions:` entry overrides the
tier for that class only; see
[the action matrix](workflow-v2.md#autonomy-tier-shorthand-and-action-matrix)
for the full grammar. The presets
declare the tier and nothing else, because a preset is a starting point,
not a policy statement about one class.

### Why three standalone documents rather than one base plus two overlays

`extends:` in workflow-v2 is **same-document only** and cross-file
`include:` is deliberately out of ADR-067 scope. A preset is a document
`fishhawk init` writes *whole* into a fresh repo that has no other
Fishhawk file to inherit from, so a preset that pointed at a base outside
itself would not be a valid spec on arrival. Each preset therefore has to
stay a complete standalone document.

That means the base-plus-deltas invariant cannot be enforced by
generation. It is enforced by **test** instead:
`TestPresetsAreLockstepBaseAndTierDelta`, present on both module sides
(`cli/internal/spec/preset_test.go` and
`backend/internal/spec/preset_test.go`). It strips only the leading
header comment block and the single `autonomy:` line from each preset,
then compares the remainders **byte for byte**. Every other comment
participates in that comparison, so tier-specific drift in a comment
fails exactly as a drifted budget would; and the test asserts the line it
stripped really was that preset's own autonomy line, so it cannot pass
vacuously on three files that share no autonomy key.

**A reader seeing three near-identical files should read that test rather
than conclude the de-duplication requirement was ignored.**

### Reuse: what is deliberately NOT hoisted

The presets declare **no `defaults` block**. Workflow-v2 same-document
reuse (`defaults` / `extends`) is fully available to an operator's own
document — it is exercised by `v2reuse_test.go` on both module sides —
but the SHIPPED presets do not use it, and the `executor` stays declared
on every stage:

```yaml
      - id: plan
        type: plan
        executor:
          agent: claude-code
```

Both Go validators resolve reuse *before* schema validation, so a
file-level `defaults.executor` would validate — an inherited executor
satisfies `$defs/stage`'s `required: [id, type, executor]` with no schema
relaxation. The reason not to ship one is the OTHER readers: a governance
document is also read by tooling that does no reuse resolution.
`fishhawk doctor`'s `execution path configured` rung parses the discovered
`.fishhawk/workflows.yaml` directly and reports **fail**, naming the
stages, when a stage carries no executor — so a freshly-scaffolded repo
whose preset relied on inheritance would fail its own readiness check. A
bare `check-jsonschema` run has the same blind spot. Per-stage executors
cost three lines and keep every reader in agreement;
`TestPresetsDeclarePerStageExecutors` pins them and fails a future edit
that hoists the block.

`reviewers` is **deliberately NOT hoisted** for a second, stronger
reason, and stays declared per stage.
It is taken WHOLE from exactly one rung and is never blended, because it
determines review AUTHORITY (ADR-027): `len(agents) > 0 && human > 0` is
advisory, `len(agents) > 0 && human == 0` is gating. The review stage
declares no reviewers on purpose. A file- or workflow-level reviewers
default would be inherited by it, silently giving it agent reviewers its
author never wrote and flipping its gating reading. This is precisely the
hazard the schema's "reviewers is taken WHOLE from exactly one rung"
rule exists for, so both module sides assert it:
`TestPresetsDoNotHoistReviewers` pins that the plan and implement stages
declare their own reviewers block and the review stage declares none.

### The handle-free approval gate

The presets carry NO fishhawk-repo-specific defaults (E36.1 / #1639) and
no `@your-github-handle` placeholder. Their approval gates use the
forge-neutral `approvals` block — `approvals: {count: 1, not: [author,
agent]}` (E39.2 / #1707; ADR-055's ratified preset default: one approval,
excluding the change's author and any agent identity) — which
workflow-v2 makes the **sole** approval predicate (E52.2 / #2214). A
freshly-scaffolded repo needs **no** top-level `roles` map and **no**
GitHub handle to fill in before its first run.

The `not: [author, agent]` exclusion is deliberate *here* because these
are authored documents. The v1→v2 migration codemod
(`fishhawk migrate-spec`) will never *invent* it when translating a
legacy `approvers` gate — the legacy form carries no source for it — and
reports that widening explicitly instead. See
[the migration doc](workflow-migration.md).

The repo's own `.fishhawk/workflows.yaml` is intentionally NOT a preset
mirror: it keeps `@kuhlman-labs` and `scripts/test verify`.

### Placeholder value every operator must replace

One value is a generic placeholder a freshly-scaffolded repo must fill in
before its first run:

| Placeholder | Where | Replace with |
|---|---|---|
| `make test` | implement stage `executor.verify.command` | Your repository's test command (run via `sh -c` after the agent exits). `verify` is optional — remove the whole block if your project has no test entrypoint — but if present, `command` must be a non-empty string. |

The placeholder is schema-valid as shipped, so a generated preset
passes validation (and `fishhawk doctor --spec-only`) before the operator
customizes it. `fishhawk doctor --spec-only` runs only the two
environment-free rungs (spec schema-validity + execution-path coverage),
so a fresh repo can be validated to the plan gate with no local Fishhawk
environment.

## Budgets

Two distinct ceilings ship, in the same USD unit.

**Workflow-level periodic ceiling** (`budgets[]`) — 50 USD per week,
`enforcement: advisory`, warning at 80%. Enforced today: it emits a
`budget_alert` audit entry and an originating-issue comment at the
`warn_at` and 100% crossings, and never blocks a run.

**Per-stage ceilings** (`budget.limit_usd`, new at workflow-v2 per E52.5
/ #2217):

| Stage | `max_runtime` | `max_tokens` | `limit_usd` |
|---|---|---|---|
| plan | `15m` | 200000 | 4 |
| implement | `30m` | 500000 | 12 |

The USD figures are calibrated from recorded run costs rather than
converted from tokens — no token-to-USD rate exists anywhere in the repo,
so inventing one would be a guess. Observed per-stage means are ~$1.4
(plan) and ~$3.9 (implement) against a whole-run maximum of ~$8.4. The
ceilings sit roughly 3x above the means, so a routine stage never
approaches them while a genuine runaway is bounded well under the weekly
ceiling.

`max_runtime` is the workflow-v2 spelling: a Go duration string replacing
v0/v1's bare-integer `max_runtime_minutes`, so a stage budget now states
its runtime cap in the same form as `policy.max_stage_runtime`,
`executor.timeout` and `executor.verify.timeout`. `max_tokens` is
retained as the optional secondary lever.

**Per-stage `limit_usd` is declarable but INERT.** No reader of a stage
budget exists in the repo yet; wiring stage-budget enforcement is #2328's
(E48.55) work. Declaring it now means an operator who scaffolds today
does not have to revisit their spec when enforcement lands.

## The generator and its delta surface

`cli/internal/spec/preset.go` implements `Generate(preset, deltas)`
(alongside the `Deltas` type and the `PresetBytes` embed accessor): it
loads the chosen preset's canonical bytes, applies structured deltas via
`yaml.v3` node edits (preserving comments and ordering — no struct
round-trip), then validates the result through the existing
`ValidateBytes` gate before returning. A delta that breaks schema
validity fails closed rather than emitting an invalid document.

The delta surface (`spec.Deltas`):

| Delta | Effect |
|---|---|
| Budget ceiling | Overrides `budgets[0].limit_usd` (the weekly advisory cost limit). The per-stage `limit_usd` ceilings are a different field and are untouched. |
| Single vs dual reviewers | Drops the Codex (`gpt-5.5`) reviewer from every stage's `reviewers.agents`, leaving Claude only. Still finds its target because reviewers stay declared per stage. |
| Human gates | Selects which of the plan / review approval gates remain human-approved (a gate not selected is left as authored). |

The generator lives in `cli/internal/spec` so `fishhawk init` stays
standalone — no backend round-trip. The backend/CLI module wall
(`backend/` and `cli/` cannot import each other's `internal/` packages)
means the generator cannot be shared; the backend side (E29.7 App-PR)
receives the mirrored embedded presets plus a validation test now —
exposed via the `PresetBytes` embed accessor in
`backend/internal/spec/preset.go` — and its own generator is deferred to
E29.7.

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
`workflow-v2` schema on BOTH sides: `cli/internal/spec/preset_test.go`
via `ValidateBytes`, `backend/internal/spec/preset_test.go` via
`ParseBytes` + `Validate` (schema + semantic). The same bytes validated
through both embed copies is the cross-boundary check that the
canonical, the CLI mirror, and the backend mirror stay in lockstep.

> Note: because the presets use no same-document reuse (see "Reuse: what
> is deliberately NOT hoisted"), a bare
> `check-jsonschema --schemafile docs/spec/workflow-v2.schema.json` run
> against one also passes. The two `ValidateBytes` / `ParseBytes` gates
> above remain the authority — they additionally run the removed-form
> sweep, reuse resolution and the semantic checks.

## Preset comments are policy-only

Preset comments explain what the operator is choosing — never which issue
or ADR shipped it. A reader scaffolding a fresh repo has no access to
this tracker, and a stale `#1234` in a governance file they now own is
noise. All provenance lives in this document; both module sides assert
the rule with `TestPresetCommentsArePolicyOnly`.
