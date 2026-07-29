# Workflow spec v2

Reference for `.fishhawk/workflows.yaml` at major version 2. The canonical schema is [`workflow-v2.schema.json`](workflow-v2.schema.json) (JSON Schema Draft 2020-12).

> **v2 *began* as a structural copy of v1 (ADR-067 / #2213; the ADR-046 major-copy precedent) and the E52 children now diverge it.** At the copy, every `$defs` entry and every non-`version` property was **validation-identical** to [`workflow-v1.schema.json`](workflow-v1.schema.json) — same keys, types, enums, `required` lists, `additionalProperties` settings, and `oneOf`/`anyOf`/`allOf` branches — with only the description strings, `$id`, `title`, and the `version` property differing. The invariant was **structural/validation equivalence, not byte identity**. It is now an **allow-list** invariant: `TestV2DivergesFromV1OnlyByLicensedDeltas` in `backend/internal/spec` walks both decoded trees and asserts the divergence set equals exactly the declared `licensedV2Deltas` table, so an accidental drop anywhere in the copy still fails while each deliberate removal is licensed to its owning issue (disposition recorded on #2320).

## What changed from v1

- **The version enum collapses to the single token `"2"`.** Unlike v0's additive `0.3…0.7` chain and v1's `1.0…1.6` chain, **v2 has no minor chain**. Declare exactly `"2"`; the dotted minor forms (`"2.0"`, `"2.1"`, …) route to this schema by major but are **rejected** by the collapsed enum.
- **Field acceptance is by schema declaration (ADR-067 scope item 4).** A field is accepted because this schema declares it — not because the document declared a high enough minor. The inherited minor-gating prose (`Requires version 0.N+`, `accepted at every advertised version`) has been rewritten to field-presence language throughout the v2 descriptions.
- **The bare `reviewer_reject` page-event token is removed (E52.3 / #2215).** See *Removed from v1* below.
- **The `reviewers.agent` integer count is removed (E52.3 / #2215).** See *Removed from v1* below.

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

## Grammar

Apart from the two removals above, the grammar is **identical to v1** — every stage type, executor branch, constraint kind, produces artifact, gate shape, delegation block, and reviewer shape carries over unchanged. For the full reference see [`workflow-v1.md`](workflow-v1.md) (the v1 deploy/acceptance/egress/agent-version/verification/diff-coverage surfaces) and [`workflow-v0.md`](workflow-v0.md) (the base grammar). A minimal v2 spec differs from a v1 spec only in its `version` value:

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

Sibling E52 children grow this page as they change the v2 grammar; for every member not listed under *Removed from v1*, the v1 reference is authoritative.

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
