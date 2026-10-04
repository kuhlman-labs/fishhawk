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

## Dispositions (`POST/GET /v0/runs/{run_id}/upkeep-dispositions`, #3923)

The captain records a per-finding verdict. Body:
`{dispositions:[{finding_id, verdict, authorize_delegation_tier?, parent_epic?}]}`,
at most 200 entries (the `findings` maxItems). `verdict` is `approved` or
`rejected`. `authorize_delegation_tier` is a BOOLEAN captain authorization for
the finding's OWN proposed `autonomy:*` label; the #3924 apply consumes it. It is
recorded verbatim and is inert when the finding proposes no tier label.
`parent_epic` overrides the proposed issue's parent and is validated with rule
(j). Both are refused on a `rejected` verdict. Each accepted entry is ONE
`upkeep_disposition_recorded` row: `{run_id, stage_id, artifact_id,
content_hash, finding_id, source, verdict, authorize_delegation_tier (always
present), parent_epic (omitempty)}`, actor `user` plus the token subject.
Repeats on one finding collapse last-wins by audit sequence, and both rows stay
in the chain.

**Binding rule.** Dispositions bind to the artifact named by the
HIGHEST-sequence `upkeep_report_recorded` row on the run's chain, never to the
newest artifact. That row is what settles the stage and carries the dedupe
verdict the apply consumes. An artifact whose recorded row never landed is not
bindable until an idempotent retry heals its row. The heal appends a new row, so
a healed older report becomes current. An undecodable newest row, an unreadable
artifact, a wrong kind or a parse failure is a 500, never a fallback to an older
report. The #3924 apply resolves through the same function
(`server.latestUpkeepReport`).

**Capture guarantee.** A capture appends its whole batch in ONE transaction
under the run-row lock. Inside that transaction it re-checks that its artifact is
still named by the highest-sequence `upkeep_report_recorded` row, and that the
artifact's window is still open. A capture that returns 200 therefore landed
against the CURRENT report, below any watermark, and the apply will consume it.
A capture that lost a race to a newer report is refused 409
`upkeep_report_superseded`, and a capture that lost a race to the apply is
refused 409 `upkeep_window_closed`. Both refusals append nothing. A capture is
NOT consumable after the fact if a later report supersedes it: the apply decides
the report that is current when it runs.

**Window.** The window for one artifact closes only when the #3924 apply appends
an artifact-bound `upkeep_apply_window_closed` watermark
(`audit.UpkeepWindowAppender.AppendChainedUpkeepWindowClose`) on approve AND
reject. Dispositions below the watermark against that artifact are the consumed
set, and the first watermark is permanent. Residual: a run that is never decided
keeps its window open. Until #3924 lands, nothing in production writes the
watermark.

| Code | HTTP | When |
|---|---|---|
| `upkeep_dispositions_unconfigured` | 503 | Run, artifact or audit repository not wired. |
| `authentication_required` | 401 | Anonymous. |
| `run_token_forbidden` | 403 | A run-bound agent token, even for its own run. |
| `operator_agent_forbidden` | 403 | A delegated operator-agent token. |
| `insufficient_scope` | 403 | Missing `write:approvals` (unconditional). |
| `validation_failed` | 400 | Bad `run_id`; unparseable body or trailing content; empty batch or more than 200 entries; empty or duplicate `finding_id`; invalid `parent_epic`; `parent_epic` or `authorize_delegation_tier: true` on `rejected`. |
| `run_not_found` | 404 | Unknown run. |
| `upkeep_verdict_invalid` | 400 | Verdict outside `{approved, rejected}` (`details.allowed`). |
| `upkeep_report_absent` | 409 | No `upkeep_report_recorded` row on the run. |
| `upkeep_finding_unknown` | 422 | A `finding_id` the bound report does not declare (`details.unknown_finding_ids`). Checked for the WHOLE batch first, so nothing is recorded. |
| `upkeep_report_superseded` | 409 | A newer report was recorded after resolution (`details.current_artifact_id`). Re-capture. |
| `upkeep_window_closed` | 409 | The apply settled this artifact's window (`artifact_id`, `settlement`, `watermark_sequence`). |
| `internal_error` | 500 | Unreadable report, or a storage failure. The atomic batch records nothing. |

`GET` requires read access only and answers 404, 409 `upkeep_report_absent` and
503 like `POST`. Both verbs return `200 {run_id, artifact_id, stage_id,
content_hash, window_closed, settlement?, dispositions:[{finding_id, source,
verdict, authorize_delegation_tier, parent_epic?, recorded_at, recorded_by,
audit_sequence}]}`, sorted by `finding_id`.

## Plan-path guard

A stage that declares `produces: upkeep_report` may ship ONLY an
`upkeep_report`, or a `clarification_request` (parking writes nothing
approvable, and an operator answer resumes the scan). The guard is an
ALLOWLIST: every other artifact kind — `plan`, `grooming_report`, and any kind
added later — is refused without being stored. When the run's workflow cannot
be resolved at all (no run row, no cached spec, no configured run repository)
the guard fails OPEN for the other kinds and the request takes today's path; a
transport error reading the run is a 500.
