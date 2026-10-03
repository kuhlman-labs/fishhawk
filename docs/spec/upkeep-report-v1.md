# Upkeep report artifact — `upkeep_report_v1`

Normative reference for the plan-stage artifact an upkeep scan ships (E79 /
#3726, contract #3921). Schema: [`upkeep-report-v1.schema.json`](upkeep-report-v1.schema.json)
(Draft 2020-12, embedded at `backend/internal/plan/schemas/` by
`scripts/sync-schemas`). Example: [`examples/upkeep-report-v1-example.json`](examples/upkeep-report-v1-example.json).
Dedupe contract and residuals: `backend/internal/upkeep/README.md`.

An upkeep report is a PROPOSAL. It lists maintenance findings — flaky tests,
toolchain drift, deprecations — each with evidence and a proposed issue. Nothing
is filed on ingest; filing is a later, gated step (#3924).

## Discrimination

`POST /v0/runs/{run_id}/plan` routes on the top-level `kind` BEFORE schema
validation. `kind: upkeep_report` selects this schema; the artifact carries no
`plan_version`, so it never collides with `plan-standard-v1`. It is the third
additive sibling after `clarification_request` and `grooming_report`.

## Top-level fields

All objects are `additionalProperties: false`.

| Field | Required | Shape |
|---|---|---|
| `kind` | yes | const `upkeep_report` |
| `report_version` | yes | const `upkeep_report_v1`; a breaking change is a new `upkeep_report_v2` file |
| `ticket_reference` | yes | `{type: github_issue, url, id}` — the run's originating ticket; verbatim `grooming-report-v1` shape |
| `generated_by` | yes | `{agent, model, version?, timestamp}` — verbatim `grooming-report-v1` shape |
| `summary` | yes | string, minLength 1 |
| `sources_scanned` | yes | array of `flake` / `toolchain_drift` / `deprecation`, minItems 1, uniqueItems |
| `findings` | yes | array of finding, maxItems 200, MAY be empty |

`sources_scanned` records what the scan COVERED. An empty `findings` array with
`sources_scanned: [flake]` reads "scanned for flakes, none found", never "not
scanned".

### Finding

| Field | Required | Shape |
|---|---|---|
| `id` | yes | pattern `^(flake\|toolchain_drift\|deprecation):[^\s<>]+$` |
| `source` | yes | `flake` / `toolchain_drift` / `deprecation` |
| `subject` | yes | 1..256 chars, pattern `^[^\s<>]+$` (test name, tool name, API) |
| `evidence` | yes | 1..50 evidence refs |
| `proposed_issue` | yes | see below |

### Evidence ref (`oneOf`, discriminated by `kind`)

- **run**: `{kind: "run", run_id, stage_id?, trace_ref?, detail?}`.
- **file**: `{kind: "file", path (minLength 1), line? (integer ≥ 1), value?, detail?}`.

### Proposed issue

`{title (1..256), body (minLength 1), type (minLength 1), labels, parent_epic?}`.
`labels` is an array of non-empty strings, uniqueItems, may be empty.
`parent_epic` is a non-empty string.

**`autonomy:*` carve-out.** A proposed `autonomy:` label is STRIPPED at apply
unless the captain authorizes it (#3924). The scan may suggest a tier; it never
sets one.

## Finding id derivation (normative)

`id = <source> + ":" + <subject>`. Derived, never minted per run, so two reports
over the same tree diff mechanically and a filed issue's marker keeps matching.
The id and subject patterns exclude whitespace and `<` / `>`.

## Hidden marker

An issue filed for a finding carries, in its body:

```
<!-- fishhawk-upkeep:v1 finding_id=<id> -->
```

The match includes the closing ` -->`, so it is exact: the marker of
`flake:TestAB` never matches `flake:TestA`. Pinned by
`TestFindingMarker_Format` (`backend/internal/upkeep`).

## Semantic rules the schema cannot express

Enforced by `plan.CheckUpkeepReportSemantics` after the schema check. Each
violation is a `*plan.SemanticError` naming the JSON pointer; ingest maps it to
`upkeep_report_invalid`.

| Rule | Pointer |
|---|---|
| (a) `id == source + ":" + subject` | `/findings/<i>/id` |
| (b) ids are unique report-wide | `/findings/<i>/id` |
| (c) every finding's `source` is in `sources_scanned` | `/findings/<i>/source` |
| (d) `subject` carries no control rune | `/findings/<i>/subject` |
| (e) a run ref's `run_id` (and `stage_id` when present) parses as a UUID and is not the nil UUID | `/findings/<i>/evidence/<j>/run_id` (`/stage_id`) |
| (f) a `flake` finding cites at least one run ref | `/findings/<i>/evidence` |
| (g) a `toolchain_drift` finding's file refs that carry a `line` name at least 2 DISTINCT paths | `/findings/<i>/evidence` |
| (h) a `deprecation` finding cites at least one file ref | `/findings/<i>/evidence` |
| (i) each label is non-empty, ≤ 50 runes, has no whitespace or control rune, and no leading or trailing punctuation or symbol rune (the `workmgmt` grooming label rule) | `/findings/<i>/proposed_issue/labels/<k>` |
| (j) `parent_epic`, when present, is a positive integer, bare or `#`-prefixed | `/findings/<i>/proposed_issue/parent_epic` |
| (k) at most 200 distinct `run_id`s across the report | `/findings` |

Evidence COUNT (minItems 1) is the schema's job and is deliberately not
re-checked by the semantic layer.

## Dedupe

On ingest each finding's proposed issue is checked against the repo's bounded
window of existing work items (`backend/internal/upkeep.MarkDuplicates`):

1. CLOSED issues never mark, by either basis.
2. **marker**: the lowest-numbered OPEN issue whose body carries the finding's
   exact marker.
3. **similarity**: otherwise the highest-scoring OPEN issue whose title scores
   `medium` or higher on `intakegroom`'s lexical similarity.

A marked finding is reported, not dropped: the apply step skips its filing. A
failed or timed-out tracker read DEGRADES to no duplicates with a named reason
(`reader_error`, `budget_exceeded`, …) and the report is still ingested.

## Ingest (`POST /v0/runs/{run_id}/plan`)

The report is bound to the stage's DECLARATION: the stage must be `plan`-typed
and the run's cached workflow must declare `produces: upkeep_report` on it.

| Code | HTTP | `details.reason` | When |
|---|---|---|---|
| `upkeep_report_stage_invalid` | 400 | `stage_type_not_plan` | The shipping stage is not `plan`-typed. Category-B. |
| `upkeep_report_stage_invalid` | 400 | `stage_does_not_declare_upkeep_report` | The stage does not declare `produces: upkeep_report`. Category-B. |
| `upkeep_report_stage_invalid` | 400 | `stage_binding_undecidable` | The run's workflow or the stage's spec entry cannot be resolved; the ingest fails CLOSED. Category-B. |
| `upkeep_report_invalid` | 400 | — | Schema or semantic-rule failure. Category-B. |
| `upkeep_report_invalid` | 400 | `run_ref_invalid` (+ `run_id`) | A cited run is unknown, in another repository, or in another account. One reason for all three. Category-B. |
| `internal_error` | 500 | — | Storage or transport failure; the stage is left untouched. |

**Run-ref tenancy.** A cited `run_id` must name a run in the reporting run's
repository (compared case-insensitively), and in the same account when BOTH
rows carry an account id; otherwise the check is repository-only. Which of
unknown / foreign it was goes to the server WARN log only.

**Unverified pointers.** Only a run ref's `run_id` is checked. Its `stage_id`
and `trace_ref` are AGENT-ASSERTED and UNVERIFIED: `stage_id` is parsed as a
UUID (rule e) but never confirmed to exist or to belong to the cited run, and
`trace_ref` is never resolved.

**Idempotency.** A byte-identical re-POST returns 200 `idempotent: true`, heals a
missing recorded row, and settles the stage; it writes no second artifact.

### `upkeep_report_recorded`

One chained audit row per persisted report:

| Key | Shape |
|---|---|
| `run_id`, `stage_id`, `artifact_id` | UUID strings |
| `content_hash`, `schema_version`, `size_bytes` | artifact identity |
| `entry_counts` | `{findings, flake, toolchain_drift, deprecation}` — every key always present |
| `duplicates` | ALWAYS a JSON array (`[]` when none or degraded); each `{finding_id, issue_number, issue_url?, basis, score?, confidence?}` |
| `dedupe_degraded` | bool, always present |
| `dedupe_degrade_reason` | present only when degraded |
| `dedupe_scanned_items`, `dedupe_window_truncated` | the window the marks were drawn from |

On success the stage settles to `awaiting_approval`.

## Plan-path guard

A stage that declares `produces: upkeep_report` may ship ONLY an
`upkeep_report`, or a `clarification_request` (parking writes nothing
approvable, and an operator answer resumes the scan). The guard is an
ALLOWLIST: every other artifact kind — `plan`, `grooming_report`, and any kind
added later — is refused without being stored. When the run's workflow cannot
be resolved at all (no run row, no cached spec, no configured run repository)
the guard fails OPEN for the other kinds and the request takes today's path; a
transport error reading the run is a 500.
