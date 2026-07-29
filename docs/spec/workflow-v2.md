# Workflow spec v2

Reference for `.fishhawk/workflows.yaml` at major version 2. The canonical schema is [`workflow-v2.schema.json`](workflow-v2.schema.json) (JSON Schema Draft 2020-12).

> **v2 *began* as a structural copy of v1 (ADR-067 / #2213; the ADR-046 major-copy precedent) and the E52 children now diverge it.** At the copy, every `$defs` entry and every non-`version` property was **validation-identical** to [`workflow-v1.schema.json`](workflow-v1.schema.json) — same keys, types, enums, `required` lists, `additionalProperties` settings, and `oneOf`/`anyOf`/`allOf` branches — with only the description strings, `$id`, `title`, and the `version` property differing. The invariant was **structural/validation equivalence, not byte identity**. It is now an **allow-list** invariant: `TestV2DivergesFromV1OnlyByLicensedDeltas` in `backend/internal/spec` walks both decoded trees and asserts the divergence set equals exactly the declared `licensedV2Deltas` table, so an accidental drop anywhere in the copy still fails while each deliberate removal is licensed to its owning issue (disposition recorded on #2320).

## What changed from v1

- **The version enum collapses to the single token `"2"`.** Unlike v0's additive `0.3…0.7` chain and v1's `1.0…1.6` chain, **v2 has no minor chain**. Declare exactly `"2"`; the dotted minor forms (`"2.0"`, `"2.1"`, …) route to this schema by major but are **rejected** by the collapsed enum.
- **Field acceptance is by schema declaration (ADR-067 scope item 4).** A field is accepted because this schema declares it — not because the document declared a high enough minor. The inherited minor-gating prose (`Requires version 0.N+`, `accepted at every advertised version`) has been rewritten to field-presence language throughout the v2 descriptions.
- **The bare `reviewer_reject` page-event token is removed (E52.3 / #2215).** See *Removed from v1* below.
- **The `reviewers.agent` integer count is removed (E52.3 / #2215).** See *Removed from v1* below.
- **Three surfaces are reshaped for legibility (E52.6 / #2218):** `constraints` becomes an object, `drive` becomes `auto_advance`, and `needs:` is added as shorthand for artifact wiring. See *Reshaped from v1* below.

## Removed from v1

v2 drops two back-compat duplicate surfaces. Each had an explicit successor already shipped in v0/v1, so the removal deletes a second way to say the same thing rather than a capability. **v0 and v1 are unchanged** — both forms remain valid there, and the shared Go types still carry them, so an existing spec keeps working until it is migrated to v2 (migration codemod: #2220).

| Removed in v2 | Replacement | Why |
|---|---|---|
| `operator_agent.must_page_human: [reviewer_reject]` | `gating_reviewer_reject` (and its sibling `advisory_reviewer_reject`) | The bare token was the pre-#1378 form and always resolved to the *gating* sense. The two explicit classes state the review authority at the declaration site instead of leaving it to be resolved. |
| `reviewers.agent: <N>` | `reviewers.agents: [{provider, model?}, …]` | The heterogeneous list (#955) already superseded the bare count — the effective agent count is `len(agents)`. Keeping both left two inputs feeding one ADR-027 authority decision. |

A v2 document using either form is rejected with a message naming the replacement, not the generic enum / `additional properties` message: the backend (`backend/internal/spec/v2removed.go`) and the CLI (`cli/internal/spec/validate.go`) each sweep the raw document for a routed major `>= 2` *before* schema validation. The sweep matches by **key name at any depth** — deliberately over-triggering rather than risk missing a legacy form — so in an already-invalid document the legacy-form message may precede a structural error.

The ADR-027 authority table now reads on `len(agents)`:

| Reviewers | Authority |
|---|---|
| `len(agents) > 0` and `human == 0` | gating (agent rejections block advancement) |
| `len(agents) > 0` and `human > 0` | advisory (agent verdicts surfaced, cannot block) |
| `len(agents) == 0` | gateless |

An absent `reviewers` block is unchanged by this slice.

## Reshaped from v1

Three surfaces change SHAPE in v2 (E52.6 / #2218). None changes what it MEANS: each is rewritten at parse time into the representation v0/v1 already use, so no consumer, no Go type, no DB column and no API field changed. **v0 and v1 are unchanged** — the old spellings remain valid there (migration codemod: #2220).

### `constraints` is an object

| v0 / v1 | v2 |
|---|---|
| a **list** of objects, each pinned to exactly one kind (`maxProperties: 1`) | **one object** keyed by kind (`maxProperties` dropped; `minProperties: 1` stays) |

```yaml
# v0 / v1
constraints:
  - max_files_changed: 45
  - forbidden_paths: ["infra/**"]

# v2
constraints:
  max_files_changed: 45
  forbidden_paths: ["infra/**"]
```

`constraints: {}` is still rejected as a no-op. A v2 document using the list form is rejected with a message showing the object form, not the generic array/object type error.

#### Why no constraint consumer changed

The obvious move — collapse the v0/v1 **list** into one canonical object with a merge rule — was **rejected**, and this is the design decision the slice turns on.

Five consumers read `[]spec.Constraint`, and they fold a duplicate-kind document **differently**: `mergeConstraints` concatenates slice kinds and takes min-wins on `max_files_changed` / max-wins on `diff_coverage`; `flattenPathConstraints` concatenates and takes min-wins; `resolveDiffCoverageConfig` takes max-wins; the deploy pre-flight gate in `approvals.go` **assigns** inside its loop, so **last wins**; and `deployEnvironmentForRun` returns the **first** non-empty entry. On one document — `[{allowed_environments: [staging]}, {allowed_environments: [prod]}]` — the two deploy sites already disagree with **each other** today: the gate permits only `prod`, the deploy record reports `staging`. A canonical merge rule cannot preserve three mutually inconsistent folds, and concatenation specifically would have **silently loosened** that governance gate by also permitting `staging`.

So the rewrite runs in the other direction, which is **exact** rather than approximate: an object's keys are unique, so it denotes exactly **one** `Constraint`, and the parser normalizes a v2 object into a **one-element list** — the single-entry case all five folds agree on. v0/v1 documents keep their list representation and are never re-resolved. The divergences are preserved by not being touched, and v2 cannot express the duplicate-kind document that makes them differ.

A consequence worth stating: because a v2 object is one `Constraint`, a binding violation is reported at `/constraints/<kind>` (e.g. `/constraints/change_freeze`) rather than at a list index that names nothing the author wrote. Below major 2 the `/constraints/<index>` form is unchanged.

### `drive` is `auto_advance`

```yaml
# v0 / v1              # v2
drive: true            auto_advance: true
```

A **pure rename of the spec surface**. The parser rewrites `auto_advance` to the `drive` key before the typed decode, so `spec.Workflow.Drive`, the `runs.drive` column, the per-run `POST /v0/runs` `drive` override and every auto-advance / `next_action` read site are untouched and see the identical value. A v2 document using `drive:` is rejected with a message naming `auto_advance`.

### `needs:` shorthand for artifact wiring

```yaml
# longhand (valid in v2 too)        # shorthand
inputs:                             needs: [plan]
  - artifact: plan
    from_stage: plan
```

Each entry names an **earlier** stage whose default artifact this stage consumes, and expands to the equivalent `inputs` entry. The artifact is derived from the **referenced** stage's type:

| Referent type | Derived artifact |
|---|---|
| `plan` | `plan` |
| `implement` | `pull_request` |
| `review`, `deploy`, `acceptance` | **rejected** — no default input artifact; declare the wiring longhand with `inputs:` |

The mapping is `$defs/input`'s artifact enum, whose only members are `plan` and `pull_request`: a review stage produces no artifact, and the `deployment` / `acceptance` artifacts are not declarable as inputs.

**`needs:` and longhand `inputs:` MAY be combined on the same stage** (the explicit decision for this slice). Ordering and dedupe:

- declared `inputs` keep their positions; derived entries follow, in `needs` order;
- a derived entry whose `(artifact, from_stage)` pair already appears verbatim among the declared inputs is **dropped**.

So the resolved input set is identical however the author spelled it.

Referent errors reuse the existing `from_stage` graph-shape rules with their unchanged messages: a referent that does not exist reports `from_stage "…" does not match any stage id`, and a self or later reference reports the must-be-earlier error. One tradeoff follows from that choice: the report names the post-expansion `inputs` index rather than the `needs` entry the author wrote. That is deliberate — one canonical error beats two competing ones — and it is recorded in `backend/internal/spec/README.md`.

> **`fishhawk validate` does NOT check `needs` referents.** `cli/internal/spec` is a deliberately separate, **schema-only** module: it performs no typed decode and no graph-shape pass at all, so it already accepts a longhand `inputs[].from_stage` naming a nonexistent stage. Both `needs`-referent errors — a nonexistent stage, and a `review` / `deploy` / `acceptance` referent with no default input artifact — therefore surface **only server-side, at run creation**, not from `fishhawk validate`. The CLI does validate the `needs` **shape** (array of stage-id-patterned strings) and rejects the two legacy spellings above with messages byte-identical to the backend's. Closing the general asymmetry needs its own decision about duplication versus coupling and is tracked on **#2323**.

## Grammar

Apart from the two removals and three reshapes above, the grammar is **identical to v1** — every stage type, executor branch, constraint kind, produces artifact, gate shape, delegation block, and reviewer shape carries over unchanged. For the full reference see [`workflow-v1.md`](workflow-v1.md) (the v1 deploy/acceptance/egress/agent-version/verification/diff-coverage surfaces) and [`workflow-v0.md`](workflow-v0.md) (the base grammar). A minimal v2 spec differs from a v1 spec only in its `version` value:

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

Sibling E52 children grow this page as they change the v2 grammar; for every member not listed under *Removed from v1* or *Reshaped from v1*, the v1 reference is authoritative.

## Version routing

The backend (`backend/internal/spec`) and the CLI (`cli/internal/spec`) compile the workflow-v0, workflow-v1, **and** workflow-v2 schemas at init and dispatch a spec to one of them by its `version` **major** component:

- `version: "0.x"` → `workflow-v0.schema.json`
- `version: "1.x"` → `workflow-v1.schema.json`
- `version: "2"` → `workflow-v2.schema.json` (a dotted `"2.0"` routes here by major but the collapsed enum rejects it)
- a missing / non-string / unparseable `version` falls through to the v0 schema, which then emits the existing required-version error (so a malformed version never silently passes)
- a well-formed but unrecognized major (`>= 3`) **fails closed** with an error naming the supported majors (`0, 1, 2`)

`/healthz` advertises the `workflow-v0`, `workflow-v1`, and `workflow-v2` embedded-schema hashes so a component can detect drift in any of them.

## See also

- [`workflow-v1.md`](workflow-v1.md) — the grammar v2 inherited, authoritative for every member v2 has not diverged.
- [`workflow-v0.md`](workflow-v0.md) — the base grammar.
- [`README.md`](README.md) — the versioning + coexistence policy.
- `docs/ARCHITECTURE.md` §10 — workflow spec grammar and version routing.
