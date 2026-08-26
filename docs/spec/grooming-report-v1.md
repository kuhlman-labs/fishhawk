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

## Runner routing

The runner does **not** validate a grooming report — it has no embedded copy of
this schema, by design (see the embedded-mirror line above and the explicit
routing comment in `scripts/sync-schemas`). The `runner` module declares no
dependency on the backend module, so `plan.DetectArtifactKind` /
`plan.ValidateArtifact` are unreachable from it; mirroring the schema plus the
semantic rules across that module wall is exactly what the no-mirror split
refuses.

What the runner does instead is **recognize the kind and get out of the way**
(#2833). `detectPlanSibling` in `runner/cmd/fishhawk-runner/main.go` peeks the
top-level `kind` against `planSiblingKinds` — the runner-side mirror of
`plan.ArtifactKindClarificationRequest` / `plan.ArtifactKindGroomingReport` —
and on a hit:

- the recognized sibling **wins over structured-output adoption**, so a plan
  captured from the agent's `structured_output` never overwrites the report on
  disk before it is uploaded;
- `plan.TryCoerce` and `plan.Validate` are **skipped**, so the report is neither
  rewritten by standard_v1-shaped coercion nor demoted to category-B for failing
  a schema it was never meant to satisfy;
- the clarification-only property stripper stays **kind-gated** — its allowlists
  are clarification-shaped and would strip every legitimate grooming property.

`uploadPlan` then re-reads the file and ships those exact bytes to
`POST /v0/runs/{run_id}/plan`, where `handleGroomingReport` validates them. On a
400 carrying `grooming_report_invalid` or `grooming_report_stage_invalid` the
runner maps the failure to **category-B**, matching the category the backend
handler has already recorded on the stage.

A THIRD sibling kind is one entry in `planSiblingKinds` plus the backend's own
routing — a known, documented residual of the module wall, not a solved problem.

### The propose stage's PROMPT states this contract (E54.28 / #2834)

The prompt a `plan`-typed grooming propose stage is served comes from
`prompt.buildGroomingPropose`, NOT `prompt.buildPlan`: it names the
`grooming_report` kind and `grooming_report_v1` version (from the
`plan.ArtifactKindGroomingReport` / `plan.GroomingReportVersion` constants), the
six-required-arrays rule, the per-ordering-entry rubric-citation requirement, the
derived-id and 1..N rank-permutation rules, and instructs the agent NOT to emit
any standard_v1 plan field. This is COMPLEMENTARY to, not a substitute for, the
schema: the prompt steers the agent toward a valid report, but the **schema —
validated by `handleGroomingReport` at ingest — remains the enforcement**. A
report that ignores the prompt still fails `grooming_report_invalid`. The prompt
branch itself fails closed (`prompt.ErrCharterNotInjected`) if no charter is
among the injected documents, so a report prompt is never served unanchored.

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

## The hygiene fix is STRUCTURED, and the prose is never dispatched

A `hygiene_defects` entry carries two fields describing its correction, and they
have different readers:

- **`suggested_fix`** is **prose for a human**. It is rendered at the operator
  gate and is **never** read as a mutation payload.
- **`fix`** is the **structured mutation payload** the apply path reads. Exactly
  the member the entry's `defect` requires is populated. It is a **suggestion
  rather than a determination**: the apply layer may refuse a proposed value on
  containment grounds and record a named skip instead of writing it (see the
  delegation-tier carve-out below).

| `defect` | mutation kind | `fix` member | example |
|---|---|---|---|
| `missing_label_namespace` | `label_set` | `fix.labels` | `["area:server-api", "phase:alpha"]` |
| `unlinked_parent_epic` | `epic_link` | `fix.parent_epic` | `"389"` or `"#389"` |
| `missing_parent_epic_link` | `epic_link` | `fix.parent_epic` | `"#389"` |
| `unboarded` | `board_place` | `fix.board_state` | `"backlog"` (a **canonical** state) |
| `missing_estimate` | `field_set` | `fix.field_value` | `"3"` |
| `absent_done_means` | *none* | *none* | the fix is authored prose; no mutation kind expresses it |

**Why the split exists (#2847).** The apply path used to assign
`strings.TrimSpace(suggested_fix)` straight into the mutation's value. A report
saying `"suggested_fix": "Add phase:alpha."` therefore asked GitHub to create a
label named literally `Add phase:alpha.` — and GitHub did, on eight real issues.
Recovering three label names from `Add area:server-api, autonomy:medium,
phase:alpha.` is a guess, and a guess that writes to a real tracker is the
failure mode this artifact's whole gate exists to prevent. So the value is
**stated**, not parsed, and the prose is unreadable by the mutator.

### Validation, and what happens when it fails

Every value is validated **before** any provider call, so a bad proposal is an
audited refusal rather than a forge error after a write was attempted:

| Member | Rule |
|---|---|
| `labels` | Each name validated **exactly as written** (never trimmed first): non-empty, carrying **no whitespace** and **no control character**, **not beginning or ending** with a punctuation or symbol rune, at most **50 characters**. So ` phase:alpha` is refused, not silently trimmed. One invalid name fails the **whole entry** — a partial label write is a half-applied fix nobody proposed. |
| `parent_epic` | A positive integer after stripping one optional leading `#`. Both wire forms (`389`, `#389`) are accepted and normalize to `#N`. |
| `board_state` | A **canonical** state, matched case-insensitively against the work-management conventions `states` map and resolved to that board's own column option before dispatch. |
| `field_value` | Single-line — checked **as written**, so a leading or trailing newline is refused rather than trimmed away — and non-empty after trimming surrounding spaces. |

The label rule is deliberately **stricter than the forge**: GitHub accepts a
label named `good first issue`. The defect class whose fix this is
(`missing_label_namespace`) is by construction a `namespace:value` label, and
the failure direction of an over-strict check is an audited skip, never a
garbage write.

An entry whose `fix` is **absent**, populates only a member this defect's kind
does not read, or carries a **blank** value is recorded as the named skip
`no_structured_fix`; one carrying an **invalid** value is recorded
`invalid_fix_value`. Both apply **nothing**, and neither falls back to
`suggested_fix` — however precisely that sentence names the value.

### A proposed delegation tier is never applied by a whole-report approval (#2855)

A label in the **`autonomy:` delegation-tier namespace** decides whether an agent
may drive the item at all. It is therefore **not** part of what a whole-report
gate approval or `hygiene: {mode: auto}` authorizes — those authorize the
clerical, reversible hygiene writes. A `fix.labels` set containing one is
recorded as the audited skip **`delegation_tier_not_authorized`** and applies
nothing; the record carries **every** proposed label on `after`, so the
groomer's suggestion stays visible at the gate and the entry resurfaces on the
next run for a human to decide.

Two consequences worth stating plainly:

- The refusal fails the **whole entry**, matching the invalid-label rule above:
  an entry proposing `["area:backend", "autonomy:low", "phase:alpha"]` applies
  **none** of the three, so the clerical halves need the human too.
- The match is on the **name**, case-insensitively and after trimming, and is
  deliberately broader than the tier parser: `autonomy:critical` is a malformed
  tier but still a tier **proposal**, so it is refused rather than parsed away.

The entry is still **reported** exactly as before — nothing filters it — and the
ingest audit payload's `entry_counts` gains a **`delegation_tier_proposals`**
key (emitted only when non-zero) so the operator can see at the gate how many
hygiene entries propose a tier and that none of them will land. Applying one
needs an explicit **per-entry** decision, which the per-entry disposition surface
(#2843) will carry.

**`fix` is optional and additive** within `grooming_report_v1`: an entry whose
defect has no mechanical mutation legitimately carries none, so it is not
`x-intended-required`. Its absence is distinguishable from an empty object on
the wire and in the decoded struct.

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

When the report carries a `milestone_scope`, eight further rules run — see
[Milestone scoping](#milestone-scoping-milestone_scope) below.

## Milestone scoping (`milestone_scope`)

`milestone_scope` is an **optional** top-level object (E54.9 / #2309). Present
only when the report scopes a release milestone; its absence means an ordinary
grooming report. It is additive within the frozen `grooming_report_v1` major, so
an existing report validates unchanged.

**The class consumes the release definition and must never invent it.**
`release_definition` carries the operator's fuzzy prose and a `source` whose only
admitted value is `operator_input` — the class scopes *against* the definition, it
does not derive one. The derivation itself (which items are in the milestone and
why) is an **agent judgment task** performed by the groom-stage agent; this
artifact ships the *contract* that judgment must satisfy — schema, typed domain,
semantic rules, ingest — exactly as #2235 did for the six existing arrays.

### Fields

| Field | Required | Notes |
|---|---|---|
| `release_definition` | yes | `{label, text, source}`; `source` is `operator_input` only. The human input, never invented. |
| `framing` | yes | `{statement, basis}` — the framing the class adopted and its architectural anchor (an ADR / architecture-doc section). The step most likely to be plausible-and-wrong, stated and grounded. |
| `included` | yes, `minItems: 0` | Items scoped IN, each with a `reason`, a `wave` index, at least one `rubric_citation`, and optional `depends_on` / `open_question_ids`. Empty is a real, different claim from an absent `milestone_scope`. |
| `excluded` | yes, `minItems: 0` | Items scoped OUT, each with a `reason` and at least one `rubric_citation`. |
| `declined_calls` | **yes, may be empty** | The ambiguous scope questions the class refused to resolve. An empty array states "nothing was ambiguous"; an absent key states nothing. |
| `critical_path` | yes | An **ordered** sequence of item keys — the longest dependency chain. |
| `edge_confidence` | yes | `{level, basis}` — confidence in the prose-derived edges. |

**Rubric citations on every entry class (C2).** `included`, `excluded` **and**
`declined_calls` each require at least one `rubric_citation`, on the same terms
`#2235` applies to ordering entries. "Why is E61 out of alpha" needs the same
charter grounding as why something is in — an exclusion carrying a bare reason is
the taste argument the rubric exists to prevent.

### Entry-id forms

Three new derived classes reuse the `#2235` contract:

| Class | Id form | Derived from |
|---|---|---|
| `milestone_inclusion` | `milestone_inclusion:<item-key>` | the item ref |
| `milestone_exclusion` | `milestone_exclusion:<item-key>` | the item ref |
| `milestone_declined` | `milestone_declined:<qualifier>` | the **zero-ref** form — `question_id`, lowercased |

The zero-ref form exists because an ambiguous scope question is not necessarily
anchored to one item, so a declined call derives its id from its `question_id`
rather than from an item ref. `question_id` must therefore be case-insensitively
unique across `declined_calls`.

### Semantic rules the schema cannot express

`plan.checkMilestoneScope` runs these after the report-wide id rules, each
returning a `*SemanticError` naming the offending JSON pointer:

1. **R1 — wave contiguity.** Waves across `included` are exactly the contiguous
   set `0..K`; a gap means the layering is not an executable sequence.
2. **R2 — dependency resolvability.** Every `depends_on` key resolves to an
   `included` or `excluded` item; a dependency on a missing item cannot be
   sequenced.
3. **R3 — strict wave monotonicity.** An in-scope dependency sits in a strictly
   earlier wave than its dependent, which makes the wave assignment a valid
   topological layering and **rejects a cycle by construction** (no strict
   layering exists for one).
4. **R4 — out-of-scope dependencies are declined, not resolved silently.** An
   in-scope item depending on an **excluded** one must carry a non-empty
   `open_question_ids` — the ambiguous call criterion 4 forbids resolving
   silently.
5. **R5 — a critical path is a PATH (C3).** Every element is an `included` item,
   elements are distinct, waves strictly increase, **and each consecutive pair is
   connected by a declared `depends_on` edge** (element *i+1* depends on element
   *i*) — so three unrelated items in waves 0/1/2 are rejected, not accepted as a
   "critical path".
6. **R6 — declined-call referential integrity.** Every `open_question_ids` id
   matches some `declined_calls[].question_id`, so a flagged ambiguity is always
   readable.
7. **R7 — canonical ordering (C4).** `included` is sorted by `(wave, item key)`,
   `excluded` by item key, `declined_calls` by `question_id`, and **every nested
   array** (`depends_on`, `open_question_ids`, `rubric_citations`, declined-call
   `options` and `item_refs`) is sorted too — so identical logical input yields
   byte-identical output. `critical_path` is the one array **not** sorted: its
   order is its content. `plan.CanonicalizeMilestoneScope` produces the canonical
   form; `plan.MilestoneScopeFingerprint` is a class-tagged sha256 over the
   canonical structural fields for a cheap run-over-run comparison. It
   fingerprints a **deep copy**, so it never mutates the supplied scope and is
   safe to call concurrently with a reader of the same scope.
8. **R8 — included/excluded disjointness.** An item may not appear in both
   `included` and `excluded`. The two derived ids differ by class prefix
   (`milestone_inclusion:<key>` vs `milestone_exclusion:<key>`), so report-wide
   id uniqueness does not catch the contradiction. This runs **before** the
   dependency rules: `checkMilestoneDependencies` resolves an `includedWave` hit
   before the `excludedKeys` branch, so a dependency on a dual-membership item
   would otherwise be treated as an ordinary in-scope edge and R4 would silently
   never fire for it.

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
