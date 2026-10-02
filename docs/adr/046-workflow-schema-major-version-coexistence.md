---
id: ADR-046
title: "Workflow schema major-version coexistence model — how workflow-v1 (deploy stage) coexists with and routes against workflow-v0"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/1380
supersedes: []
superseded_by: []
applies_to: ["docs/spec/workflow-v1.schema.json", "backend/internal/spec/schemas/workflow-v1.schema.json", "cli/internal/spec/schemas/workflow-v1.schema.json", "backend/internal/spec/**"]
---

# ADR-046: Workflow schema major-version coexistence model — how workflow-v1 (deploy stage) coexists with and routes against workflow-v0

**Status:** **Accepted — 2026-06-27** (operator-ratified). This ADR records the `v0 ↔ v1` versioning gate that ADR-038 (#925) names as the prerequisite for the first E23.x `workflow-v1` deploy-schema slice (epic #924).

## Context

ADR-038 (#925, Accepted 2026-06-27) adopts a `deploy` stage that requires a **`workflow-v1` bump** — the first *major, non-additive* change to the workflow spec. Until now every change has been additive within `workflow-v0.x` (currently v0.7, #1378): new optional fields / enum values added to the single `docs/spec/workflow-v0.schema.json`, mirrored to `backend/internal/spec/schemas/` and `cli/internal/spec/schemas/` by `scripts/sync-schemas`, advertised via the schema's `version` enum + the `/healthz` embedded-schema hash. `deploy` (stage `type`) and `deployment` (`produces` artifact) extend sets that `docs/spec/workflow-v0.md` documents as **frozen closed sets**, so they cannot be additive — hence v1.

ADR-038 commits to a coexistence property without specifying the mechanism: *"the backend accepts v0 and v1 specs simultaneously during the deprecation window (the version string routes each spec to its schema/execution path)."* **This ADR decides _how_** — the major-version coexistence/routing model — because it is the prerequisite ADR-038 names for the first E23.x schema slice, and it sets the precedent for every future major bump.

What exists today (grounding):
- One canonical schema `docs/spec/workflow-v0.schema.json` + 2 embedded mirrors; `scripts/sync-schemas`' `workflow-v*` case copies to exactly those two.
- `version` is a string enum inside that one schema; the recognized workflow-version set lives in the schema enum, **not** in `backend/internal/plan` (that recognizes plan-artifact versions like `standard_v1`).
- `/healthz` advertises `schemas["workflow-v0"]` as the embedded-schema hash.
- Spec parse/validate is `backend/internal/spec` against a single `spec.Workflow` Go shape.
- Precedent: `workflow-v0.x` enum additions are **not** `if/then` version-gated (the #1378 / v0.6 pattern); the operator-role contract (#1026) uses a base `operator-role.schema.json` + a separate `operator-role-overlay.schema.json`.

## Options

1. **Separate schema files per major version.** Add `docs/spec/workflow-v1.schema.json` (+ its 2 mirrors) alongside the v0 schema; the backend selects the validator by the spec's `version` (`0.x` → v0 schema, `1.x` → v1 schema). Go: one superset `spec.Workflow` (deploy fields `omitempty`, rejected by the v0 validator) or a version-tagged shape. *Pro:* clean closed-set separation per ADR-038's framing, each version independently frozen, `/healthz` advertises both hashes. *Con:* duplication of the shared stage shell + a validator-routing layer + a second `sync-schemas` target.

2. **Single evolving schema with version-gated constructs.** Keep one schema; add `deploy`/`deployment` but `if/then`-gate them on `version >= 1.0`. *Pro:* no duplication. *Con:* breaks the "frozen closed set per version" framing inside one file; introduces `if/then` version gating the repo has deliberately avoided (v0.x additions are ungated); harder to reason about which members are valid at which version.

3. **Hard cutover to v1 (no coexistence).** Bump everything to v1, migrate the in-repo spec, drop v0. *Pro:* simplest schema story. *Con:* breaks ADR-038's explicit "accept v0 and v1 simultaneously during a deprecation window" commitment and any external v0 specs; rejected unless coexistence is deemed unnecessary.

4. **Schema family: shared core + per-version overlay.** A base schema for the common stage shell + per-major-version overlays adding that version's closed-set members, reusing the established `operator-role.schema.json` + `-overlay.schema.json` pattern (#1026). *Pro:* less duplication than (1), proven in-repo pattern. *Con:* overlay composition for a top-level workflow schema is more machinery than two flat files; the validator still routes by version.

## Recommendation

Lean **option 1 (separate per-major-version schema files + version-routed validator)** as the clearest and most precedent-setting, borrowing option 4's shared-core idea only if the duplication proves painful. It keeps each major version's closed sets independently frozen (matching ADR-038's framing), makes `/healthz` advertise `workflow-v0` and `workflow-v1` hashes side by side, and isolates v1's lifecycle/state-machine differences behind a clean version boundary. Defer the Go-representation sub-choice (superset struct vs. version-tagged union) to the first schema slice's plan.

Open sub-questions to settle in the Decision:
- **Version routing key:** route on the major prefix of the `version` string (`0.` vs `1.`)? Where does selection live (the `backend/internal/spec` parse entry)?
- **Go shape:** one superset `spec.Workflow` (deploy fields rejected by the v0 validator) vs. two shapes.
- **sync-schemas / /healthz:** `scripts/sync-schemas` gains a `workflow-v1` target; `/healthz` advertises both hashes; the CLAUDE.md "Version advertising" checklist gains v1.
- **Deprecation-window policy:** how long both validate; is there a soak analogous to the additive-field soak; what deprecates v0 and when.
- **`.fishhawk/workflows.yaml`:** stays `v0.x` until it declares a `deploy` stage; the in-repo dogfood spec is not force-migrated.

## Decision

**Adopted: option 1 — separate per-major-version schema files + a version-routed validator.** Concretely:

1. **Separate per-major-version schema files.** Add a new canonical `docs/spec/workflow-v1.schema.json` alongside `docs/spec/workflow-v0.schema.json`; each is independently frozen with its own closed sets (v1's stage-`type` set adds `deploy`; its `produces` set adds `deployment`). `scripts/sync-schemas` gains a `workflow-v1` target mirroring to `backend/internal/spec/schemas/workflow-v1.schema.json` and `cli/internal/spec/schemas/workflow-v1.schema.json`. v0 is **not** edited to know about deploy — the closed-set freeze per ADR-038 is honored by keeping the new members out of the v0 file entirely.

2. **Route on the major component of the `version` string.** The `backend/internal/spec` parse entry reads `version` first and dispatches: `0.x` → v0 schema/validator, `1.x` → v1 schema/validator. The dispatch is a single switch at parse time; an unrecognized major **fails closed** with an actionable error naming the supported majors (never a silent accept). The recognized-version set stays expressed by each schema's `version` enum (v0 enum: `0.1`…`0.7`; v1 enum: `1.0`), not a separate code table — consistent with how workflow versions are recognized today.

3. **No intra-file `if/then` version gating.** The major boundary is the file/validator split, not conditional subschemas inside one file. This preserves the established convention (the #1378 / v0.6 pattern adds enum values *without* `if/then` gates) and keeps "what is legal at version N" answerable by reading one file.

4. **Go shape: one superset `spec.Workflow` struct; legality enforced by the per-version schema.** Keep a single Go type with the deploy additions (`deploy` stage fields, `deployment` artifact) as `omitempty`; the JSON Schema is the source of truth for per-version legality — the v0 schema rejects a `deploy` stage, the v1 schema accepts it. This avoids a struct fork / version-tagged union while keeping each version correctly validated. The first schema slice's plan may revisit if the superset struct gets unwieldy (escape hatch: a version-tagged shape), but starts with the superset.

5. **Version advertising: advertise both.** `/healthz` `schemas` carries **both** `workflow-v0` and `workflow-v1` embedded-schema hashes; the CLAUDE.md "Version advertising" checklist + the `## Build, test, lint` schema notes gain the v1 schema + its two mirrors. The schema-sync CI gate covers the new mirror set.

6. **Coexistence/deprecation policy: both validate indefinitely; this ADR schedules no v0 sunset.** v0 stays the default and is opt-out only by declaring `version: "1.0"`. New optional fields continue additively within each major line (`v0.x`, `v1.x`) per the existing soak convention. Any future v0 deprecation/sunset is a separate decision when/if warranted.

7. **`.fishhawk/workflows.yaml` stays on `v0.x`** until it declares a `deploy` stage; the in-repo dogfood spec is not force-migrated. The first adopter of v1 is whoever adds a deploy stage.

**Precedent this sets:** every future major workflow-spec bump = a new `workflow-vN.schema.json` (+ 2 mirrors + `/healthz` hash + a new arm in the version-routing switch), each major's closed sets independently frozen, coexisting until a separate ADR sunsets an old major.

**Rationale over the alternatives:** option 2 (single schema + `if/then`) breaks the per-version closed-set freeze inside one file and reintroduces the version-gating the repo deliberately avoids; option 3 (hard cutover) breaks ADR-038's explicit simultaneous-acceptance commitment; option 4 (base + overlay) is viable but more composition machinery than two flat files buy us — adopt its shared-core idea later only if v0/v1 duplication proves painful.

## Consequences

- Sets the precedent for every future major workflow-spec bump, not just deploy.
- Adds a validator-selection/routing layer in `backend/internal/spec` and a second schema + mirror set; `scripts/sync-schemas`, `/healthz` advertising, and the CLAUDE.md version-advertising checklist all gain a `workflow-v1` entry.
- Unblocks the first E23.x slice (the `workflow-v1` deploy schema) — ADR-038 (#925) names this as its prerequisite.
- The chosen model bounds how much v0↔v1 drift is tolerable and how external v0 specs are treated through the deprecation window.

Related: ADR-038 (#925, the deploy stage requiring v1), epic #924 (E23 deploy stage), #1378 (workflow-v0.7 additive precedent + the no-if/then-gate convention), #1026 (operator-role base+overlay schema pattern), `scripts/sync-schemas`, `docs/spec/workflow-v0.md`.
