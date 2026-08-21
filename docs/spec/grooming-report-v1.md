# Grooming report artifact — `grooming_report_v1`

The second additive sibling of the plan artifact (E54.3 / #2235, ADR-065 §3).
A `plan`-typed **propose** stage running the backlog-grooming workflow emits a
grooming report instead of a plan: a set of typed **proposals** over a backlog
slice, which a human reviews at the plan gate and decides on. Nothing in this
artifact mutates a tracker.

- **Canonical schema**: [`grooming-report-v1.schema.json`](grooming-report-v1.schema.json)
- **Embedded mirror**: `backend/internal/plan/schemas/grooming-report-v1.schema.json` (synced by `scripts/sync-schemas`; the runner does **not** embed it — the runner writes the report, the backend validates it on ingest)
- **Example**: [`examples/grooming-report-v1-example.json`](examples/grooming-report-v1-example.json)
- **Go domain + validator**: `backend/internal/plan/groomingreport.go`
- **Ingest**: `POST /v0/runs/{run_id}/plan` (discriminated on the top-level `kind`), handled by `backend/internal/server/grooming_report.go`

## Discrimination

`kind: "grooming_report"` is the top-level discriminator. `plan.DetectArtifactKind`
routes on it **before** validation, so `plan-standard-v1.schema.json` (frozen,
`additionalProperties: false`) is never consulted for a grooming report and vice
versa. There is exactly one owner of that routing —
`plan.DetectArtifactKind` / `plan.ValidateArtifact` — and this artifact is its
second additive sibling, following `clarification_request`.

## Top-level fields

| Field | Required | Notes |
|---|---|---|
| `kind` | yes | const `grooming_report`. |
| `report_version` | yes | const `grooming_report_v1`. |
| `ticket_reference` | yes | The ticket the grooming **run** was opened against. Shape mirrors `plan-standard-v1` verbatim. |
| `generated_by` | yes | Agent / model / timestamp. Shape mirrors `plan-standard-v1` verbatim. |
| `summary` | yes | Lead paragraph rendered first at the operator gate. |
| `charter_ref` | **optional**, `x-intended-required` | `{path, content_hash, ref?}` — the exact charter revision the report was scored against. Optional in `grooming_report_v1` only because the charter resolution/injection machinery is E54.2 / #2234's slice; this artifact must not block on it. Draft 2020-12 §10.3 collects `x-intended-required` as an annotation, so declaring the intent changes no validation result. |
| `ordering` | yes, `minItems: 1` | The proposed ranking. |
| `duplicates` | yes | May be `[]`. |
| `hygiene_defects` | yes | May be `[]`. |
| `dependency_edges` | yes | May be `[]`. |
| `vision_drift` | yes | May be `[]`. |
| `decomposition_suggestions` | yes | May be `[]`. |

All six entry arrays are **required** even when empty. `[]` states "none found",
which is a different claim from omitting the key — and the two must not be
conflated by a downstream diff (#2240).

## The citation rule is a schema constraint, not a prompt

Charter §5.1: *"Every ranking entry names the rubric id justifying it. A score
with no citation fails validation (#2235)."*

That is enforced here, in the schema, by two keywords on `ordering-entry`:
`rubric_citations` appears in `required`, **and** carries `minItems: 1`. An
ordering entry that omits the key, or ships `[]`, is **REJECTED** — it is never
accepted-but-undecorated.

Why it lives in the schema rather than in the planner prompt: the grooming report
is the input to an operator gate whose whole value is that a human can check the
reasoning. A report that can propose an unjustified ranking turns that gate into
rubber-stamping, and rubber-stamping erodes the control every other gate rests on
(ADR-065's charter-specificity mitigation; charter §5.4). A prompt is advice; a
schema `required` is a refusal.

## Entry-id derivation (normative)

Every entry carries a stable `id`. The id is **derived** from the underlying
backlog item and the proposal class — it is never minted per run. That is what
makes #2240's run-over-run diff mechanical: the **same** finding on the **same**
item yields the **same** id on a later run, so a diff of two reports separates
*new / gone / changed* without heuristics.

`plan.GroomingEntryID(class, qualifier string, refs ...ItemRef) string` is the
single owner of this derivation, and `plan.ValidateGroomingReport` **machine-checks**
it: every entry's declared id must recompose from that entry's own fields. A
per-run-minted id cannot recompose, so it is rejected.

### Item key

The item key is the provider discriminator, a `/`, then the item's provider-native
id, **lowercased**:

| `item_ref.type` | Item key | Example |
|---|---|---|
| `github_issue` | `github/<owner>/<repo>#<number>` | `github/kuhlman-labs/fishhawk#2235` |
| `gitlab_issue` | `gitlab/<namespace>/<project>#<number>` | `gitlab/acme/platform#17` |
| `jira_issue` | `jira/<project>-<number>` | `jira/plat-412` |

**The provider discriminator is part of the key (F3).** An earlier draft used the
bare `<owner>/<repo>#<number>` form. That has two defects an id scheme whose whole
job is to be a stable join key cannot carry: it is **undefined** for Jira (which
has no owner/repo), and it **collides** across forges — `acme/platform#17` on
GitHub and on GitLab are different items that would map to one key, which is not
hypothetical now that GitLab support has shipped. So the schema admits exactly
three `item_ref.type` values and each has a defined, discriminated derivation.
**Never widen the `item_ref.type` enum without adding the matching case to
`GroomingEntryID`** — a schema that admits input its own contract cannot process
is worse than one that rejects it.

### Why the item-key alphabet excludes the separators

`item_ref.id`'s character class is `^[A-Za-z0-9][A-Za-z0-9._/#-]*$`. It excludes
**both** structural separators an entry id uses:

- `:` — splits `<class>:<item-key>:<qualifier>`;
- `+` — splits the two keys of a `duplicate` pair or a `dependency` edge id.

Excluding `+` is **load-bearing, not cosmetic**. A pair/edge id is a *flat string
join* of two keys, so if a key could itself contain the join character then two
**different** pairs could derive the **same** id: `{"a", "b+github/c"}` and
`{"a+github/b", "c"}` both join to `duplicate:github/a+github/b+github/c`. That is
not a parse-ambiguity nit — nothing ever splits an id, so parsing is not the
concern. **Cross-entry and cross-run id collision is.** The id is the join key
across report → operator decision → applied mutation → audit, so a collision would
(a) make report-wide uniqueness spuriously reject a report carrying both pairs and
(b) let #2240's run-over-run diff conflate two distinct proposals under one id —
the same silent-wrong-answer shape the directional-dependency rule below exists to
prevent. No admitted provider needs `+`: GitHub/GitLab ids are
`<owner>/<repo>#<number>` and Jira ids are `<PROJECT>-<number>`.

The **entry-id** pattern still admits `+` in its key segment — that is where the
separator legitimately appears, between two keys.

`plan.GroomingEntryID` adds a second, independent layer: it percent-escapes `%`,
`:` and `+` when building a key (`itemKeyEscaper`), which makes the key an
**injective** encoding of `(provider, id)` for *any* input, not only for
schema-valid input. That matters because #2240 derives ids straight from tracker
rows rather than from an already-validated report. For every schema-valid id the
escaper is a no-op, so derived ids are unchanged.

### Id forms

| Class | Id form | Qualifier |
|---|---|---|
| `ordering` | `ordering:<item-key>` | — |
| `decomposition` | `decomposition:<item-key>` | — |
| `hygiene` | `hygiene:<item-key>:<qualifier>` | the `defect` enum value |
| `vision_drift` | `vision_drift:<item-key>:<qualifier>` | the `charter_ref_id` |
| `duplicate` | `duplicate:<key-a>+<key-b>` | — (keys sorted lexicographically) |
| `dependency` | `dependency:<from-key>+<to-key>` | — (keys **not** sorted) |

### Why duplicate sorts and dependency does not (F1)

A **duplicate pair is an unordered relation**: "A and B overlap" is the same
proposal as "B and A overlap". Sorting the two keys makes the id
orientation-stable, so the same pair yields the same id however the groomer
happened to order it.

A **dependency edge is directional**: `A depends_on B` and `B depends_on A` are
different proposals with **opposite meaning**. Sorting there would collapse them
onto one id, with two concrete consequences, both bad:

1. Report-wide id uniqueness (below) would forbid a single report from ever
   proposing both directions.
2. #2240's run-over-run diff could not distinguish a run that **flipped** an edge
   from an unchanged one — the flip would surface as a content change under an
   identical id, which is exactly the silent-wrong-answer shape this artifact is
   meant to make impossible.

So the dependency id encodes **from-key then to-key, unsorted**, and A→B and B→A
are two ids that coexist in one report without tripping uniqueness.

### Qualifier case (F4)

The entry-id pattern's qualifier segment is **lowercase-only**
(`[a-z0-9][a-z0-9._-]*`), while charter line ids and rubric ids are conventionally
uppercase (the shipped charter uses `V1`–`V5`, `R1`–`R5`, `U1`–`U4`, `S1`–`S5`).
Three places therefore state the same rule, deliberately:

1. the **schema pattern** admits only a lowercase qualifier;
2. `GroomingEntryID` **lowercases** the qualifier when deriving;
3. the **recomposition check** compares lowercased.

Consequence, stated so it is not discovered later: two charter ids differing only
by case (`t4` and `T4`) map to **one** qualifier. **Charter line ids must therefore
be case-insensitively unique.** The hygiene `defect` qualifiers are already
lowercase by construction (they are the schema enum values).

## Semantic rules the schema cannot express

`plan.ValidateGroomingReport` runs these after schema validation, each returning a
`*SemanticError` naming the offending id:

1. **Entry ids are unique across the whole report.** The id is the join key across
   report → operator decision → applied mutation → audit, so a duplicate is
   ambiguous exactly as a duplicate clarification-question id is.
2. **An entry's id class prefix matches the array it appears in.** An `ordering:`
   id inside `duplicates` is a routing bug, not a naming choice.
3. **An entry's id recomposes** from its own `item_ref` / `pair` / `from`+`to` and
   qualifier via `GroomingEntryID`. This is the control that makes run-over-run
   stability mechanical rather than aspirational.
4. **A `duplicates` pair and a `dependency_edges` edge relate two DISTINCT
   items.** Endpoint identity is the derived **item key**, not the raw
   `item_ref` object, so two refs differing only in `url` or in `id` casing are
   the same endpoint here exactly as they are in the id. Every other rule
   accepts the degenerate case — `duplicate:<k>+<k>` and `dependency:<k>+<k>`
   recompose from their own refs and are unique — so without this rule "item A
   duplicates item A" and "item A depends_on item A" would reach the operator
   gate, and #2240's diff, as proposals with no actionable content.
5. **The `ordering` ranks are exactly the permutation `1..N`.** A duplicated or
   gapped rank is not an applicable proposal.

## Workflow declaration

`grooming_report` is a `produces` artifact at **workflow-v2 only** (v0 and v1 are
frozen majors and keep rejecting it). Its bindings, enforced by
`backend/internal/spec` `Validate`:

- valid **only** on a `plan`-typed stage — the PROPOSE stage per ADR-067 §2 —
  mirroring the `deployment`→deploy and `acceptance`→acceptance bindings;
- a `grooming_report`-producing stage MUST declare `schema: grooming_report_v1`,
  mirroring the `plan`/`standard_v1` rule and MVP_SPEC §4.3's
  forward-compatibility argument;
- one stage may not declare **both** `plan` and `grooming_report`: a propose stage
  proposes one thing.

```yaml
- id: groom
  type: plan
  produces:
    - artifact: grooming_report
      schema: grooming_report_v1
```

## Persistence and audit

The ingest handler persists the report as an `artifact` row with
`kind = grooming_report`, `schema_version = grooming_report_v1`, and the sha256
content hash (migration 0073 widens `artifacts_kind_check` to admit the kind),
then appends a chained `grooming_report_recorded` audit entry carrying
`{run_id, stage_id, artifact_id, content_hash, schema_version, size_bytes,
entry_counts}`. The content hash is the durable pointer — #2240 reads the report
body by hash; the per-class `entry_counts` are the cheap run-over-run signal.

Idempotency is `GetByHash` on `(stage_id, content_hash)`. That path **verifies the
audit entry exists and appends it when missing** (`ensureGovernanceAuditEntry`,
#1396): a create-then-append where the append failed would otherwise leave the
report durable and the audit chain permanently silent about it, and the retry
would short-circuit to success without ever healing the gap.

The existence check and the writes it authorizes are **one critical section**
(`groomingIngestMu`), so two concurrent identical POSTs produce exactly one
artifact row and one audit entry. Without it the handler holds two check-then-act
windows — two first-POSTs can both miss `GetByHash` and both create, and a retry
can slip between the original's `Create` and its append, heal the "missing" entry,
and leave the original to append a second one. The lock is **process-local**: two
`fishhawkd` replicas racing the same report can still double-write. Closing that
needs DB-level dedup on `(stage_id, content_hash)` governing every artifact kind,
which is a schema change beyond this artifact's slice; the same residual is
documented on `ensureGovernanceAuditEntry`.

## Error codes

| Code | HTTP | Meaning |
|---|---|---|
| `grooming_report_stage_invalid` | 400 | Shipped from a stage whose type is not `plan`. Fails the stage category-B — re-shipping the same bytes cannot help. |
| `grooming_report_invalid` | 400 | Fails `grooming-report-v1` schema validation or one of the five semantic rules. Category-B. |

## Size

The report rides `POST /v0/runs/{run_id}/plan` and is subject to that endpoint's
existing body cap: an oversized report is refused with `413 body_too_large` rather
than silently truncated. Raising the cap, if real grooming runs need it, is a
separate measured change.
